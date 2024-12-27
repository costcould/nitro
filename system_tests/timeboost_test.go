package arbtest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/arbos/arbostypes"
	"github.com/offchainlabs/nitro/arbos/util"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/broadcastclient"
	"github.com/offchainlabs/nitro/broadcaster/message"
	"github.com/offchainlabs/nitro/cmd/genericconf"
	"github.com/offchainlabs/nitro/execution/gethexec"
	"github.com/offchainlabs/nitro/pubsub"
	"github.com/offchainlabs/nitro/solgen/go/express_lane_auctiongen"
	"github.com/offchainlabs/nitro/solgen/go/mocksgen"
	"github.com/offchainlabs/nitro/timeboost"
	"github.com/offchainlabs/nitro/timeboost/bindings"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/containers"
	"github.com/offchainlabs/nitro/util/redisutil"
	"github.com/offchainlabs/nitro/util/rpcclient"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/offchainlabs/nitro/util/testhelpers"
)

func dbKey(prefix []byte, pos uint64) []byte {
	var key []byte
	key = append(key, prefix...)
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, pos)
	key = append(key, data...)
	return key
}

func TestTimeboostBulkBlockMetadataFetcher(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder := NewNodeBuilder(ctx).DefaultConfig(t, true)
	httpConfig := genericconf.HTTPConfigDefault
	httpConfig.Addr = "127.0.0.1"
	httpConfig.Apply(builder.l2StackConfig)
	builder.execConfig.BlockMetadataApiCacheSize = 0 // Caching is disabled
	builder.nodeConfig.TransactionStreamer.TrackBlockMetadataFrom = 1
	cleanupSeq := builder.Build(t)
	defer cleanupSeq()

	// Generate blocks until current block is > 20
	arbDb := builder.L2.ConsensusNode.ArbDB
	builder.L2Info.GenerateAccount("User")
	user := builder.L2Info.GetDefaultTransactOpts("User", ctx)
	var latestL2 uint64
	var err error
	var lastTx *types.Transaction
	for i := 0; ; i++ {
		lastTx, _ = builder.L2.TransferBalanceTo(t, "Owner", util.RemapL1Address(user.From), big.NewInt(1e18), builder.L2Info)
		latestL2, err = builder.L2.Client.BlockNumber(ctx)
		Require(t, err)
		// Clean BlockMetadata from arbDB so that we can modify it at will
		Require(t, arbDb.Delete(dbKey([]byte("t"), latestL2)))
		if latestL2 > uint64(20) {
			break
		}
	}
	var sampleBulkData []arbostypes.BlockMetadata
	for i := 1; i <= int(latestL2); i++ {
		// #nosec G115
		blockMetadata := []byte{0, uint8(i)}
		sampleBulkData = append(sampleBulkData, blockMetadata)
		// #nosec G115
		Require(t, arbDb.Put(dbKey([]byte("t"), uint64(i)), blockMetadata))
	}

	nodecfg := arbnode.ConfigDefaultL1NonSequencerTest()
	trackBlockMetadataFrom := uint64(5)
	nodecfg.TransactionStreamer.TrackBlockMetadataFrom = trackBlockMetadataFrom
	newNode, cleanupNewNode := builder.Build2ndNode(t, &SecondNodeParams{
		nodeConfig:  nodecfg,
		stackConfig: testhelpers.CreateStackConfigForTest(t.TempDir()),
	})
	defer cleanupNewNode()

	// Wait for second node to catchup via L1, since L1 doesn't have the blockMetadata, we ensure that messages are tracked with missingBlockMetadataInputFeedPrefix prefix
	_, err = WaitForTx(ctx, newNode.Client, lastTx.Hash(), time.Second*5)
	Require(t, err)

	blockMetadataInputFeedPrefix := []byte("t")
	missingBlockMetadataInputFeedPrefix := []byte("x")
	arbDb = newNode.ConsensusNode.ArbDB

	// Introduce fragmentation
	blocksWithBlockMetadata := []uint64{8, 9, 10, 14, 16}
	for _, key := range blocksWithBlockMetadata {
		Require(t, arbDb.Put(dbKey([]byte("t"), key), sampleBulkData[key-1]))
		Require(t, arbDb.Delete(dbKey([]byte("x"), key)))
	}

	// Check if all block numbers with missingBlockMetadataInputFeedPrefix are present as keys in arbDB and that no keys with blockMetadataInputFeedPrefix
	iter := arbDb.NewIterator(blockMetadataInputFeedPrefix, nil)
	pos := uint64(0)
	for iter.Next() {
		keyBytes := bytes.TrimPrefix(iter.Key(), blockMetadataInputFeedPrefix)
		if binary.BigEndian.Uint64(keyBytes) != blocksWithBlockMetadata[pos] {
			t.Fatalf("unexpected presence of blockMetadata, when blocks are synced via L1. msgSeqNum: %d, expectedMsgSeqNum: %d", binary.BigEndian.Uint64(keyBytes), blocksWithBlockMetadata[pos])
		}
		pos++
	}
	iter.Release()
	iter = arbDb.NewIterator(missingBlockMetadataInputFeedPrefix, nil)
	pos = trackBlockMetadataFrom
	i := 0
	for iter.Next() {
		// Blocks with blockMetadata present shouldn't have the missingBlockMetadataInputFeedPrefix keys present in arbDB
		for i < len(blocksWithBlockMetadata) && blocksWithBlockMetadata[i] == pos {
			i++
			pos++
		}
		keyBytes := bytes.TrimPrefix(iter.Key(), missingBlockMetadataInputFeedPrefix)
		if binary.BigEndian.Uint64(keyBytes) != pos {
			t.Fatalf("unexpected msgSeqNum with missingBlockMetadataInputFeedPrefix for blockMetadata. Want: %d, Got: %d", pos, binary.BigEndian.Uint64(keyBytes))
		}
		pos++
	}
	if pos-1 != latestL2 {
		t.Fatalf("number of keys with missingBlockMetadataInputFeedPrefix doesn't match expected value. Want: %d, Got: %d", latestL2, pos-1)
	}
	iter.Release()

	// Rebuild blockMetadata and cleanup trackers from ArbDB
	blockMetadataFetcher, err := arbnode.NewBlockMetadataFetcher(ctx, arbnode.BlockMetadataFetcherConfig{Source: rpcclient.ClientConfig{URL: builder.L2.Stack.HTTPEndpoint()}}, arbDb, newNode.ExecNode)
	Require(t, err)
	blockMetadataFetcher.Update(ctx)

	// Check if all blockMetadata was synced from bulk BlockMetadata API via the blockMetadataFetcher and that trackers for missing blockMetadata were cleared
	iter = arbDb.NewIterator(blockMetadataInputFeedPrefix, nil)
	pos = trackBlockMetadataFrom
	for iter.Next() {
		keyBytes := bytes.TrimPrefix(iter.Key(), blockMetadataInputFeedPrefix)
		if binary.BigEndian.Uint64(keyBytes) != pos {
			t.Fatalf("unexpected msgSeqNum with blockMetadataInputFeedPrefix for blockMetadata. Want: %d, Got: %d", pos, binary.BigEndian.Uint64(keyBytes))
		}
		if !bytes.Equal(sampleBulkData[pos-1], iter.Value()) {
			t.Fatalf("blockMetadata mismatch for blockNumber: %d. Want: %v, Got: %v", pos, sampleBulkData[pos-1], iter.Value())
		}
		pos++
	}
	if pos-1 != latestL2 {
		t.Fatalf("number of keys with blockMetadataInputFeedPrefix doesn't match expected value. Want: %d, Got: %d", latestL2, pos-1)
	}
	iter.Release()
	iter = arbDb.NewIterator(missingBlockMetadataInputFeedPrefix, nil)
	for iter.Next() {
		keyBytes := bytes.TrimPrefix(iter.Key(), missingBlockMetadataInputFeedPrefix)
		t.Fatalf("unexpected presence of msgSeqNum with missingBlockMetadataInputFeedPrefix, indicating missing of some blockMetadata after rebuilding. msgSeqNum: %d", binary.BigEndian.Uint64(keyBytes))
	}
	iter.Release()
}

func TestTimeboostBulkBlockMetadataAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	builder := NewNodeBuilder(ctx).DefaultConfig(t, false)
	builder.execConfig.BlockMetadataApiCacheSize = 0 // Caching is disabled
	cleanup := builder.Build(t)
	defer cleanup()

	arbDb := builder.L2.ConsensusNode.ArbDB

	// Generate blocks until current block is end
	start := 1
	end := 20
	builder.L2Info.GenerateAccount("User")
	user := builder.L2Info.GetDefaultTransactOpts("User", ctx)
	for i := 0; ; i++ {
		builder.L2.TransferBalanceTo(t, "Owner", util.RemapL1Address(user.From), big.NewInt(1e18), builder.L2Info)
		latestL2, err := builder.L2.Client.BlockNumber(ctx)
		Require(t, err)
		// Clean BlockMetadata from arbDB so that we can modify it at will
		Require(t, arbDb.Delete(dbKey([]byte("t"), latestL2)))
		// #nosec G115
		if latestL2 > uint64(end)+10 {
			break
		}
	}
	var sampleBulkData []gethexec.NumberAndBlockMetadata
	for i := start; i <= end; i += 2 {
		sampleData := gethexec.NumberAndBlockMetadata{
			// #nosec G115
			BlockNumber: uint64(i),
			// #nosec G115
			RawMetadata: []byte{0, uint8(i)},
		}
		sampleBulkData = append(sampleBulkData, sampleData)
		Require(t, arbDb.Put(dbKey([]byte("t"), sampleData.BlockNumber), sampleData.RawMetadata))
	}

	l2rpc := builder.L2.Stack.Attach()
	var result []gethexec.NumberAndBlockMetadata
	err := l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), "latest") // Test rpc.BlockNumber feature, send "latest" as an arg instead of blockNumber
	Require(t, err)

	if len(result) != len(sampleBulkData) {
		t.Fatalf("number of entries in arb_getRawBlockMetadata is incorrect. Got: %d, Want: %d", len(result), len(sampleBulkData))
	}
	for i, data := range result {
		if data.BlockNumber != sampleBulkData[i].BlockNumber {
			t.Fatalf("BlockNumber mismatch. Got: %d, Want: %d", data.BlockNumber, sampleBulkData[i].BlockNumber)
		}
		if !bytes.Equal(data.RawMetadata, sampleBulkData[i].RawMetadata) {
			t.Fatalf("RawMetadata. Got: %s, Want: %s", data.RawMetadata, sampleBulkData[i].RawMetadata)
		}
	}

	// Test that without cache the result returned is always in sync with ArbDB
	sampleBulkData[0].RawMetadata = []byte{1, 11}
	Require(t, arbDb.Put(dbKey([]byte("t"), 1), sampleBulkData[0].RawMetadata))

	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(1), rpc.BlockNumber(1))
	Require(t, err)
	if len(result) != 1 {
		t.Fatal("result returned with more than one entry")
	}
	if !bytes.Equal(sampleBulkData[0].RawMetadata, result[0].RawMetadata) {
		t.Fatal("BlockMetadata gotten from API doesn't match the latest entry in ArbDB")
	}

	// Test that LRU caching works
	builder.execConfig.BlockMetadataApiCacheSize = 1000
	builder.execConfig.BlockMetadataApiBlocksLimit = 25
	builder.RestartL2Node(t)
	l2rpc = builder.L2.Stack.Attach()
	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), rpc.BlockNumber(end))
	Require(t, err)

	arbDb = builder.L2.ConsensusNode.ArbDB
	updatedBlockMetadata := []byte{2, 12}
	Require(t, arbDb.Put(dbKey([]byte("t"), 1), updatedBlockMetadata))

	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(1), rpc.BlockNumber(1))
	Require(t, err)
	if len(result) != 1 {
		t.Fatal("result returned with more than one entry")
	}
	if bytes.Equal(updatedBlockMetadata, result[0].RawMetadata) {
		t.Fatal("BlockMetadata should've been fetched from cache and not the db")
	}
	if !bytes.Equal(sampleBulkData[0].RawMetadata, result[0].RawMetadata) {
		t.Fatal("incorrect caching of BlockMetadata")
	}

	// Test that ErrBlockMetadataApiBlocksLimitExceeded is thrown when query range exceeds the limit
	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), rpc.BlockNumber(26))
	if !strings.Contains(err.Error(), gethexec.ErrBlockMetadataApiBlocksLimitExceeded.Error()) {
		t.Fatalf("expecting ErrBlockMetadataApiBlocksLimitExceeded error, got: %v", err)
	}

	// A Reorg event should clear the cache, hence the data fetched now should be accurate
	Require(t, builder.L2.ConsensusNode.TxStreamer.ReorgTo(10))
	err = l2rpc.CallContext(ctx, &result, "arb_getRawBlockMetadata", rpc.BlockNumber(start), rpc.BlockNumber(end))
	Require(t, err)
	if !bytes.Equal(updatedBlockMetadata, result[0].RawMetadata) {
		t.Fatal("BlockMetadata should've been fetched from db and not the cache")
	}
}

func TestExpressLaneControlTransfer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})
	jwtSecretPath := filepath.Join(tmpDir, "sequencer.jwt")

	seq, seqClient, seqInfo, auctionContractAddr, aliceBidderClient, bobBidderClient, roundDuration, cleanupSeq, _, cleanupFeedListener := setupExpressLaneAuction(t, tmpDir, ctx, jwtSecretPath)
	defer cleanupSeq()
	defer cleanupFeedListener()

	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(auctionContractAddr, seqClient)
	Require(t, err)
	rawRoundTimingInfo, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	roundTimingInfo, err := timeboost.NewRoundTimingInfo(rawRoundTimingInfo)
	Require(t, err)

	// Prepare clients that can submit txs to the sequencer via the express lane.
	chainId, err := seqClient.ChainID(ctx)
	Require(t, err)
	seqDial, err := rpc.Dial(seq.Stack.HTTPEndpoint())
	Require(t, err)
	createExpressLaneClientFor := func(name string) (*expressLaneClient, bind.TransactOpts) {
		priv := seqInfo.Accounts[name].PrivateKey
		expressLaneClient := newExpressLaneClient(
			priv,
			chainId,
			*roundTimingInfo,
			auctionContractAddr,
			seqDial,
		)
		expressLaneClient.Start(ctx)
		transacOpts := seqInfo.GetDefaultTransactOpts(name, ctx)
		transacOpts.NoSend = true
		return expressLaneClient, transacOpts
	}
	bobExpressLaneClient, bobOpts := createExpressLaneClientFor("Bob")
	aliceExpressLaneClient, aliceOpts := createExpressLaneClientFor("Alice")

	// Bob will win the auction and become controller for next round
	placeBidsAndDecideWinner(t, ctx, seqClient, seqInfo, auctionContract, "Bob", "Alice", bobBidderClient, aliceBidderClient, roundDuration)
	time.Sleep(roundTimingInfo.TimeTilNextRound())

	// Check that Bob's tx gets priority since he's the controller
	verifyControllerAdvantage(t, ctx, seqClient, bobExpressLaneClient, seqInfo, "Bob", "Alice")

	// Transfer express lane control from Bob to Alice
	currRound := roundTimingInfo.RoundNumber()
	duringRoundTransferTx, err := auctionContract.ExpressLaneAuctionTransactor.TransferExpressLaneController(&bobOpts, currRound, seqInfo.Accounts["Alice"].Address)
	Require(t, err)
	err = bobExpressLaneClient.SendTransaction(ctx, duringRoundTransferTx)
	Require(t, err)

	time.Sleep(time.Second) // Wait for controller to change on the sequencer side
	// Check that now Alice's tx gets priority since she's the controller after bob transfered it
	verifyControllerAdvantage(t, ctx, seqClient, aliceExpressLaneClient, seqInfo, "Alice", "Bob")

	// Alice and Bob submit bids and Alice wins for the next round
	placeBidsAndDecideWinner(t, ctx, seqClient, seqInfo, auctionContract, "Alice", "Bob", aliceBidderClient, bobBidderClient, roundDuration)
	t.Log("Alice won the express lane auction for upcoming round, now try to transfer control before the next round begins...")

	// Alice now transfers control to bob before her round begins
	winnerRound := currRound + 1
	currRound = roundTimingInfo.RoundNumber()
	if currRound >= winnerRound {
		t.Fatalf("next round already began, try running the test again. Current round: %d, Winner Round: %d", currRound, winnerRound)
	}

	beforeRoundTransferTx, err := auctionContract.ExpressLaneAuctionTransactor.TransferExpressLaneController(&aliceOpts, winnerRound, seqInfo.Accounts["Bob"].Address)
	Require(t, err)
	err = aliceExpressLaneClient.SendTransaction(ctx, beforeRoundTransferTx)
	Require(t, err)

	setExpressLaneIterator, err := auctionContract.FilterSetExpressLaneController(&bind.FilterOpts{Context: ctx}, nil, nil, nil)
	Require(t, err)
	verifyControllerChange := func(round uint64, prev, new common.Address) {
		setExpressLaneIterator.Next()
		if setExpressLaneIterator.Event.Round != round {
			t.Fatalf("unexpected round number. Want: %d, Got: %d", round, setExpressLaneIterator.Event.Round)
		}
		if setExpressLaneIterator.Event.PreviousExpressLaneController != prev {
			t.Fatalf("unexpected previous express lane controller. Want: %v, Got: %v", prev, setExpressLaneIterator.Event.PreviousExpressLaneController)
		}
		if setExpressLaneIterator.Event.NewExpressLaneController != new {
			t.Fatalf("unexpected new express lane controller. Want: %v, Got: %v", new, setExpressLaneIterator.Event.NewExpressLaneController)
		}
	}
	// Verify during round control change
	verifyControllerChange(currRound, common.Address{}, bobOpts.From) // Bob wins auction
	verifyControllerChange(currRound, bobOpts.From, aliceOpts.From)   // Bob transfers control to Alice
	// Verify before round control change
	verifyControllerChange(winnerRound, common.Address{}, aliceOpts.From) // Alice wins auction
	verifyControllerChange(winnerRound, aliceOpts.From, bobOpts.From)     // Alice transfers control to Bob before the round begins
}

func TestSequencerFeed_ExpressLaneAuction_ExpressLaneTxsHaveAdvantage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})
	jwtSecretPath := filepath.Join(tmpDir, "sequencer.jwt")

	seq, seqClient, seqInfo, auctionContractAddr, aliceBidderClient, bobBidderClient, roundDuration, cleanupSeq, _, cleanupFeedListener := setupExpressLaneAuction(t, tmpDir, ctx, jwtSecretPath)
	defer cleanupSeq()
	defer cleanupFeedListener()

	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(auctionContractAddr, seqClient)
	Require(t, err)
	rawRoundTimingInfo, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	roundTimingInfo, err := timeboost.NewRoundTimingInfo(rawRoundTimingInfo)
	Require(t, err)

	placeBidsAndDecideWinner(t, ctx, seqClient, seqInfo, auctionContract, "Bob", "Alice", bobBidderClient, aliceBidderClient, roundDuration)
	time.Sleep(roundTimingInfo.TimeTilNextRound())

	chainId, err := seqClient.ChainID(ctx)
	Require(t, err)

	// Prepare a client that can submit txs to the sequencer via the express lane.
	bobPriv := seqInfo.Accounts["Bob"].PrivateKey
	seqDial, err := rpc.Dial(seq.Stack.HTTPEndpoint())
	Require(t, err)
	expressLaneClient := newExpressLaneClient(
		bobPriv,
		chainId,
		*roundTimingInfo,
		auctionContractAddr,
		seqDial,
	)
	expressLaneClient.Start(ctx)

	verifyControllerAdvantage(t, ctx, seqClient, expressLaneClient, seqInfo, "Bob", "Alice")
}

func TestSequencerFeed_ExpressLaneAuction_InnerPayloadNoncesAreRespected_TimeboostedFieldIsCorrect(t *testing.T) {
	t.Parallel()

	// logHandler := testhelpers.InitTestLog(t, log.LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})
	jwtSecretPath := filepath.Join(tmpDir, "sequencer.jwt")
	seq, seqClient, seqInfo, auctionContractAddr, aliceBidderClient, bobBidderClient, roundDuration, cleanupSeq, feedListener, cleanupFeedListener := setupExpressLaneAuction(t, tmpDir, ctx, jwtSecretPath)
	defer cleanupSeq()
	defer cleanupFeedListener()

	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(auctionContractAddr, seqClient)
	Require(t, err)
	rawRoundTimingInfo, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	roundTimingInfo, err := timeboost.NewRoundTimingInfo(rawRoundTimingInfo)
	Require(t, err)

	Require(t, err)

	placeBidsAndDecideWinner(t, ctx, seqClient, seqInfo, auctionContract, "Bob", "Alice", bobBidderClient, aliceBidderClient, roundDuration)
	time.Sleep(roundTimingInfo.TimeTilNextRound())

	// Prepare a client that can submit txs to the sequencer via the express lane.
	bobPriv := seqInfo.Accounts["Bob"].PrivateKey
	chainId, err := seqClient.ChainID(ctx)
	Require(t, err)
	seqDial, err := rpc.Dial(seq.Stack.HTTPEndpoint())
	Require(t, err)
	expressLaneClient := newExpressLaneClient(
		bobPriv,
		chainId,
		*roundTimingInfo,
		auctionContractAddr,
		seqDial,
	)
	expressLaneClient.Start(ctx)

	// We first generate an account for Charlie and transfer some balance to him.
	seqInfo.GenerateAccount("Charlie")
	TransferBalance(t, "Owner", "Charlie", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)

	// During the express lane, Bob sends txs that do not belong to him, but he is the express lane controller so they
	// will go through the express lane.
	// These tx payloads are sent with nonces out of order, and those with nonces too high should fail.
	var wg sync.WaitGroup
	wg.Add(2)
	ownerAddr := seqInfo.GetAddress("Owner")
	aliceNonce, err := seqClient.PendingNonceAt(ctx, seqInfo.GetAddress("Alice"))
	Require(t, err)
	aliceData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		GasFeeCap: new(big.Int).Set(seqInfo.GasPrice),
		Value:     big.NewInt(1e12),
		Nonce:     aliceNonce,
		Data:      nil,
	}
	aliceTx := seqInfo.SignTxAs("Alice", aliceData)
	go func(w *sync.WaitGroup) {
		defer w.Done()
		err = seqClient.SendTransaction(ctx, aliceTx)
		Require(t, err)
	}(&wg)

	txData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		Value:     big.NewInt(1e12),
		Nonce:     1,
		GasFeeCap: aliceTx.GasFeeCap(),
		Data:      nil,
	}
	charlie1 := seqInfo.SignTxAs("Charlie", txData)
	txData = &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		Value:     big.NewInt(1e12),
		Nonce:     0,
		GasFeeCap: aliceTx.GasFeeCap(),
		Data:      nil,
	}
	charlie0 := seqInfo.SignTxAs("Charlie", txData)
	var err2 error
	go func(w *sync.WaitGroup) {
		defer w.Done()
		time.Sleep(time.Millisecond * 10)
		// Send the express lane txs with nonces out of order
		err2 = expressLaneClient.SendTransaction(ctx, charlie1)
		err = expressLaneClient.SendTransaction(ctx, charlie0)
		Require(t, err)
	}(&wg)
	wg.Wait()
	if err2 == nil {
		t.Fatal("Charlie should not be able to send tx with nonce 1")
	}
	// After round is done, verify that Charlie beats Alice in the final sequence, and that the emitted txs
	// for Charlie are correct.
	aliceReceipt, err := seqClient.TransactionReceipt(ctx, aliceTx.Hash())
	Require(t, err)
	aliceBlock := aliceReceipt.BlockNumber.Uint64()
	charlieReceipt, err := seqClient.TransactionReceipt(ctx, charlie0.Hash())
	Require(t, err)
	charlieBlock := charlieReceipt.BlockNumber.Uint64()

	if aliceBlock < charlieBlock {
		t.Fatal("Alice's tx should not have been sequenced before Charlie's in different blocks")
	} else if aliceBlock == charlieBlock {
		if aliceReceipt.TransactionIndex < charlieReceipt.TransactionIndex {
			t.Fatal("Charlie should have been sequenced before Alice with express lane")
		}
	}

	// First test that timeboosted byte array is correct on sequencer side
	verifyTimeboostedCorrectness(t, ctx, "Alice", seq, seqClient, false, aliceTx, aliceBlock)
	verifyTimeboostedCorrectness(t, ctx, "Charlie", seq, seqClient, true, charlie0, charlieBlock)

	// Verify that timeboosted byte array receieved via sequencer feed is correct
	_, err = WaitForTx(ctx, feedListener.Client, charlie0.Hash(), time.Second*5)
	Require(t, err)
	_, err = WaitForTx(ctx, feedListener.Client, aliceTx.Hash(), time.Second*5)
	Require(t, err)
	verifyTimeboostedCorrectness(t, ctx, "Alice", feedListener.ConsensusNode, feedListener.Client, false, aliceTx, aliceBlock)
	verifyTimeboostedCorrectness(t, ctx, "Charlie", feedListener.ConsensusNode, feedListener.Client, true, charlie0, charlieBlock)

	// arbnode.BlockHashMismatchLogMsg has been randomly appearing and disappearing when running this test, not sure why that might be happening
	// if logHandler.WasLogged(arbnode.BlockHashMismatchLogMsg) {
	// 	t.Fatal("BlockHashMismatchLogMsg was logged unexpectedly")
	// }
}

// verifyTimeboostedCorrectness is used to check if the timeboosted byte array in both the sequencer's tx streamer and the client node's tx streamer (which is connected
// to the sequencer feed) is accurate, i.e it represents correctly whether a tx is timeboosted or not
func verifyTimeboostedCorrectness(t *testing.T, ctx context.Context, user string, tNode *arbnode.Node, tClient *ethclient.Client, isTimeboosted bool, userTx *types.Transaction, userTxBlockNum uint64) {
	blockMetadataOfBlock, err := tNode.TxStreamer.BlockMetadataAtCount(arbutil.MessageIndex(userTxBlockNum) + 1)
	Require(t, err)
	if len(blockMetadataOfBlock) == 0 {
		t.Fatal("got empty blockMetadata byte array")
	}
	if blockMetadataOfBlock[0] != message.TimeboostedVersion {
		t.Fatalf("blockMetadata byte array has invalid version. Want: %d, Got: %d", message.TimeboostedVersion, blockMetadataOfBlock[0])
	}
	userTxBlock, err := tClient.BlockByNumber(ctx, new(big.Int).SetUint64(userTxBlockNum))
	Require(t, err)
	var foundUserTx bool
	for txIndex, tx := range userTxBlock.Transactions() {
		got, err := blockMetadataOfBlock.IsTxTimeboosted(txIndex)
		Require(t, err)
		if tx.Hash() == userTx.Hash() {
			foundUserTx = true
			if !isTimeboosted && got {
				t.Fatalf("incorrect timeboosted bit for %s's tx, it shouldn't be timeboosted", user)
			} else if isTimeboosted && !got {
				t.Fatalf("incorrect timeboosted bit for %s's tx, it should be timeboosted", user)
			}
		} else if got {
			// Other tx's right now shouln't be timeboosted
			t.Fatalf("incorrect timeboosted bit for nonspecified tx with index: %d, it shouldn't be timeboosted", txIndex)
		}
	}
	if !foundUserTx {
		t.Fatalf("%s's tx wasn't found in the block with blockNum retrieved from its receipt", user)
	}
}

func placeBidsAndDecideWinner(t *testing.T, ctx context.Context, seqClient *ethclient.Client, seqInfo *BlockchainTestInfo, auctionContract *express_lane_auctiongen.ExpressLaneAuction, winner, loser string, winnerBidderClient, loserBidderClient *timeboost.BidderClient, roundDuration time.Duration) {
	t.Helper()

	rawRoundTimingInfo, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	roundTimingInfo, err := timeboost.NewRoundTimingInfo(rawRoundTimingInfo)
	Require(t, err)
	currRound := roundTimingInfo.RoundNumber()

	// We are now in the bidding round, both issue their bids. winner will win
	t.Logf("%s and %s now submitting their bids at %v", winner, loser, time.Now())
	winnerBid, err := winnerBidderClient.Bid(ctx, big.NewInt(2), seqInfo.GetAddress(winner))
	Require(t, err)
	loserBid, err := loserBidderClient.Bid(ctx, big.NewInt(1), seqInfo.GetAddress(loser))
	Require(t, err)
	t.Logf("%s bid %+v", winner, winnerBid)
	t.Logf("%s bid %+v", loser, loserBid)

	// Subscribe to auction resolutions and wait for a winner
	winnerAddr, winnerRound := awaitAuctionResolved(t, ctx, seqClient, auctionContract)

	// Verify winner wins the auction
	if winnerAddr != seqInfo.GetAddress(winner) {
		t.Fatalf("%s should have won the express lane auction", winner)
	}
	t.Logf("%s won the auction for the round: %d", winner, winnerRound)
	if winnerRound != currRound+1 {
		t.Fatalf("unexpected winner round: Want:%d Got:%d", currRound+1, winnerRound)
	}

	it, err := auctionContract.FilterAuctionResolved(&bind.FilterOpts{Context: ctx}, nil, nil, nil)
	Require(t, err)
	winnerWon := false
	for it.Next() {
		if it.Event.FirstPriceBidder == seqInfo.GetAddress(winner) && it.Event.Round == winnerRound {
			winnerWon = true
		}
	}
	if !winnerWon {
		t.Fatalf("%s should have won the auction", winner)
	}
}

func verifyControllerAdvantage(t *testing.T, ctx context.Context, seqClient *ethclient.Client, controllerClient *expressLaneClient, seqInfo *BlockchainTestInfo, controller, otherUser string) {
	t.Helper()

	// During the express lane around, controller sends txs always 150ms later than otherUser, but otherUser's
	// txs end up getting delayed by 200ms as they are not the express lane controller.
	// In the end, controller's txs should be ordered before otherUser's during the round.
	var wg sync.WaitGroup
	wg.Add(2)
	ownerAddr := seqInfo.GetAddress("Owner")

	otherUserNonce, err := seqClient.PendingNonceAt(ctx, seqInfo.GetAddress(otherUser))
	Require(t, err)
	otherUserData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		GasFeeCap: new(big.Int).Set(seqInfo.GasPrice),
		Value:     big.NewInt(1e12),
		Nonce:     otherUserNonce,
		Data:      nil,
	}
	otherUserTx := seqInfo.SignTxAs(otherUser, otherUserData)
	go func(w *sync.WaitGroup) {
		defer w.Done()
		Require(t, seqClient.SendTransaction(ctx, otherUserTx))
	}(&wg)

	controllerNonce, err := seqClient.PendingNonceAt(ctx, seqInfo.GetAddress(controller))
	Require(t, err)
	controllerData := &types.DynamicFeeTx{
		To:        &ownerAddr,
		Gas:       seqInfo.TransferGas,
		GasFeeCap: new(big.Int).Set(seqInfo.GasPrice),
		Value:     big.NewInt(1e12),
		Nonce:     controllerNonce,
		Data:      nil,
	}
	controllerBoostableTx := seqInfo.SignTxAs(controller, controllerData)
	go func(w *sync.WaitGroup) {
		defer w.Done()
		time.Sleep(time.Millisecond * 10)
		Require(t, controllerClient.SendTransaction(ctx, controllerBoostableTx))
	}(&wg)
	wg.Wait()

	// After round is done, verify that controller beats otherUser in the final sequence.
	otherUserTxReceipt, err := seqClient.TransactionReceipt(ctx, otherUserTx.Hash())
	Require(t, err)
	otherUserBlock := otherUserTxReceipt.BlockNumber.Uint64()
	controllerBoostableTxReceipt, err := seqClient.TransactionReceipt(ctx, controllerBoostableTx.Hash())
	Require(t, err)
	controllerBlock := controllerBoostableTxReceipt.BlockNumber.Uint64()

	if otherUserBlock < controllerBlock {
		t.Fatalf("%s's tx should not have been sequenced before %s's in different blocks", otherUser, controller)
	} else if otherUserBlock == controllerBlock {
		if otherUserTxReceipt.TransactionIndex < controllerBoostableTxReceipt.TransactionIndex {
			t.Fatalf("%s should have been sequenced before %s with express lane", controller, otherUser)
		}
	}
}

func setupExpressLaneAuction(
	t *testing.T,
	dbDirPath string,
	ctx context.Context,
	jwtSecretPath string,
) (*arbnode.Node, *ethclient.Client, *BlockchainTestInfo, common.Address, *timeboost.BidderClient, *timeboost.BidderClient, time.Duration, func(), *TestClient, func()) {

	builderSeq := NewNodeBuilder(ctx).DefaultConfig(t, true)

	seqPort := getRandomPort(t)
	seqAuthPort := getRandomPort(t)
	builderSeq.l2StackConfig.HTTPHost = "localhost"
	builderSeq.l2StackConfig.HTTPPort = seqPort
	builderSeq.l2StackConfig.HTTPModules = []string{"eth", "arb", "debug", "timeboost"}
	builderSeq.l2StackConfig.AuthPort = seqAuthPort
	builderSeq.l2StackConfig.AuthModules = []string{"eth", "arb", "debug", "timeboost", "auctioneer"}
	builderSeq.l2StackConfig.JWTSecret = jwtSecretPath
	builderSeq.nodeConfig.Feed.Output = *newBroadcasterConfigTest()
	builderSeq.execConfig.Sequencer.Enable = true
	builderSeq.execConfig.Sequencer.Timeboost = gethexec.TimeboostConfig{
		Enable:               false, // We need to start without timeboost initially to create the auction contract
		ExpressLaneAdvantage: time.Second * 5,
	}
	builderSeq.nodeConfig.TransactionStreamer.TrackBlockMetadataFrom = 1
	cleanupSeq := builderSeq.Build(t)
	seqInfo, seqNode, seqClient := builderSeq.L2Info, builderSeq.L2.ConsensusNode, builderSeq.L2.Client

	tcpAddr, ok := seqNode.BroadcastServer.ListenerAddr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("failed to cast listener address to *net.TCPAddr")
	}
	port := tcpAddr.Port
	builderFeedListener := NewNodeBuilder(ctx).DefaultConfig(t, true)
	builderFeedListener.isSequencer = false
	builderFeedListener.nodeConfig.Feed.Input = *newBroadcastClientConfigTest(port)
	builderFeedListener.nodeConfig.Feed.Input.Timeout = broadcastclient.DefaultConfig.Timeout
	cleanupFeedListener := builderFeedListener.Build(t)

	// Send an L2 tx in the background every two seconds to keep the chain moving.
	go func() {
		tick := time.NewTicker(time.Second * 2)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				tx := seqInfo.PrepareTx("Owner", "Owner", seqInfo.TransferGas, big.NewInt(1), nil)
				err := seqClient.SendTransaction(ctx, tx)
				t.Log("Failed to send test tx", err)
			}
		}
	}()

	// Set up the auction contracts on L2.
	// Deploy the express lane auction contract and erc20 to the parent chain.
	ownerOpts := seqInfo.GetDefaultTransactOpts("Owner", ctx)
	erc20Addr, tx, erc20, err := bindings.DeployMockERC20(&ownerOpts, seqClient)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Initialize(&ownerOpts, "LANE", "LNE", 18)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	// Fund the auction contract.
	seqInfo.GenerateAccount("AuctionContract")
	TransferBalance(t, "Owner", "AuctionContract", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)

	// Mint some tokens to Alice and Bob.
	seqInfo.GenerateAccount("Alice")
	seqInfo.GenerateAccount("Bob")
	TransferBalance(t, "Faucet", "Alice", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)
	TransferBalance(t, "Faucet", "Bob", arbmath.BigMulByUint(oneEth, 500), seqInfo, seqClient, ctx)
	aliceOpts := seqInfo.GetDefaultTransactOpts("Alice", ctx)
	bobOpts := seqInfo.GetDefaultTransactOpts("Bob", ctx)
	tx, err = erc20.Mint(&ownerOpts, aliceOpts.From, big.NewInt(100))
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Mint(&ownerOpts, bobOpts.From, big.NewInt(100))
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	// Calculate the number of seconds until the next minute
	// and the next timestamp that is a multiple of a minute.
	now := time.Now()
	roundDuration := time.Minute
	// Correctly calculate the remaining time until the next minute
	waitTime := roundDuration - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())*time.Nanosecond
	// Get the current Unix timestamp at the start of the minute
	initialTimestamp := big.NewInt(now.Add(waitTime).Unix())
	initialTimestampUnix := time.Unix(initialTimestamp.Int64(), 0)

	// Deploy the auction manager contract.
	auctionContractAddr, tx, _, err := express_lane_auctiongen.DeployExpressLaneAuction(&ownerOpts, seqClient)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	proxyAddr, tx, _, err := mocksgen.DeploySimpleProxy(&ownerOpts, seqClient, auctionContractAddr)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(proxyAddr, seqClient)
	Require(t, err)

	auctioneerAddr := seqInfo.GetDefaultTransactOpts("AuctionContract", ctx).From
	beneficiary := auctioneerAddr
	biddingToken := erc20Addr
	bidRoundSeconds := uint64(60)
	auctionClosingSeconds := uint64(15)
	reserveSubmissionSeconds := uint64(15)
	minReservePrice := big.NewInt(1) // 1 wei.
	roleAdmin := auctioneerAddr
	tx, err = auctionContract.Initialize(
		&ownerOpts,
		express_lane_auctiongen.InitArgs{
			Auctioneer:   auctioneerAddr,
			BiddingToken: biddingToken,
			Beneficiary:  beneficiary,
			RoundTimingInfo: express_lane_auctiongen.RoundTimingInfo{
				OffsetTimestamp:          initialTimestamp.Int64(),
				RoundDurationSeconds:     bidRoundSeconds,
				AuctionClosingSeconds:    auctionClosingSeconds,
				ReserveSubmissionSeconds: reserveSubmissionSeconds,
			},
			MinReservePrice:       minReservePrice,
			AuctioneerAdmin:       roleAdmin,
			MinReservePriceSetter: roleAdmin,
			ReservePriceSetter:    roleAdmin,
			BeneficiarySetter:     roleAdmin,
			RoundTimingSetter:     roleAdmin,
			MasterAdmin:           roleAdmin,
		},
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	t.Log("Deployed all the auction manager stuff", auctionContractAddr)
	// We approve the spending of the erc20 for the autonomous auction contract and bid receiver
	// for both Alice and Bob.
	bidReceiverAddr := common.HexToAddress("0x2424242424242424242424242424242424242424")
	maxUint256 := big.NewInt(1)
	maxUint256.Lsh(maxUint256, 256).Sub(maxUint256, big.NewInt(1))

	tx, err = erc20.Approve(
		&aliceOpts, proxyAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Approve(
		&aliceOpts, bidReceiverAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Approve(
		&bobOpts, proxyAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}
	tx, err = erc20.Approve(
		&bobOpts, bidReceiverAddr, maxUint256,
	)
	Require(t, err)
	if _, err = bind.WaitMined(ctx, seqClient, tx); err != nil {
		t.Fatal(err)
	}

	// This is hacky- we are manually starting the ExpressLaneService here instead of letting it be started
	// by the sequencer. This is due to needing to deploy the auction contract first.
	builderSeq.execConfig.Sequencer.Timeboost.Enable = true
	builderSeq.L2.ExecNode.Sequencer.StartExpressLane(ctx, builderSeq.L2.ExecNode.Backend.APIBackend(), builderSeq.L2.ExecNode.FilterSystem, proxyAddr, seqInfo.GetAddress("AuctionContract"), gethexec.DefaultTimeboostConfig.EarlySubmissionGrace)
	t.Log("Started express lane service in sequencer")

	// Set up an autonomous auction contract service that runs in the background in this test.
	redisURL := redisutil.CreateTestRedis(ctx, t)

	// Set up the auctioneer RPC service.
	bidValidatorPort := getRandomPort(t)
	bidValidatorWsPort := getRandomPort(t)
	stackConf := node.Config{
		DataDir:             "", // ephemeral.
		HTTPPort:            bidValidatorPort,
		HTTPHost:            "localhost",
		HTTPModules:         []string{timeboost.AuctioneerNamespace},
		HTTPVirtualHosts:    []string{"localhost"},
		HTTPTimeouts:        rpc.DefaultHTTPTimeouts,
		WSHost:              "localhost",
		WSPort:              bidValidatorWsPort,
		WSModules:           []string{timeboost.AuctioneerNamespace},
		GraphQLVirtualHosts: []string{"localhost"},
		P2P: p2p.Config{
			ListenAddr:  "",
			NoDial:      true,
			NoDiscovery: true,
		},
	}
	stack, err := node.New(&stackConf)
	Require(t, err)
	cfg := &timeboost.BidValidatorConfig{
		SequencerEndpoint:      fmt.Sprintf("http://localhost:%d", seqPort),
		AuctionContractAddress: proxyAddr.Hex(),
		RedisURL:               redisURL,
		ProducerConfig:         pubsub.TestProducerConfig,
	}
	fetcher := func() *timeboost.BidValidatorConfig {
		return cfg
	}
	bidValidator, err := timeboost.NewBidValidator(
		ctx, stack, fetcher,
	)
	Require(t, err)
	Require(t, stack.Start())
	Require(t, bidValidator.Initialize(ctx))
	bidValidator.Start(ctx)

	auctioneerCfg := &timeboost.AuctioneerServerConfig{
		SequencerEndpoint:      fmt.Sprintf("http://localhost:%d", seqAuthPort),
		AuctionContractAddress: proxyAddr.Hex(),
		RedisURL:               redisURL,
		ConsumerConfig:         pubsub.TestConsumerConfig,
		SequencerJWTPath:       jwtSecretPath,
		DbDirectory:            dbDirPath,
		Wallet: genericconf.WalletConfig{
			PrivateKey: fmt.Sprintf("00%x", seqInfo.Accounts["AuctionContract"].PrivateKey.D.Bytes()),
		},
	}
	auctioneerFetcher := func() *timeboost.AuctioneerServerConfig {
		return auctioneerCfg
	}
	am, err := timeboost.NewAuctioneerServer(
		ctx,
		auctioneerFetcher,
	)
	Require(t, err)
	am.Start(ctx)

	// Set up a bidder client for Alice and Bob.
	alicePriv := seqInfo.Accounts["Alice"].PrivateKey
	cfgFetcherAlice := func() *timeboost.BidderClientConfig {
		return &timeboost.BidderClientConfig{
			AuctionContractAddress: proxyAddr.Hex(),
			BidValidatorEndpoint:   fmt.Sprintf("http://localhost:%d", bidValidatorPort),
			ArbitrumNodeEndpoint:   fmt.Sprintf("http://localhost:%d", seqPort),
			Wallet: genericconf.WalletConfig{
				PrivateKey: fmt.Sprintf("00%x", alicePriv.D.Bytes()),
			},
		}
	}
	alice, err := timeboost.NewBidderClient(
		ctx,
		cfgFetcherAlice,
	)
	Require(t, err)

	bobPriv := seqInfo.Accounts["Bob"].PrivateKey
	cfgFetcherBob := func() *timeboost.BidderClientConfig {
		return &timeboost.BidderClientConfig{
			AuctionContractAddress: proxyAddr.Hex(),
			BidValidatorEndpoint:   fmt.Sprintf("http://localhost:%d", bidValidatorPort),
			ArbitrumNodeEndpoint:   fmt.Sprintf("http://localhost:%d", seqPort),
			Wallet: genericconf.WalletConfig{
				PrivateKey: fmt.Sprintf("00%x", bobPriv.D.Bytes()),
			},
		}
	}
	bob, err := timeboost.NewBidderClient(
		ctx,
		cfgFetcherBob,
	)
	Require(t, err)

	alice.Start(ctx)
	bob.Start(ctx)

	// Wait until the initial round.
	timeToWait := time.Until(initialTimestampUnix)
	t.Logf("Waiting until the initial round %v and %v, current time %v", timeToWait, initialTimestampUnix, time.Now())
	<-time.After(timeToWait)

	t.Log("Started auction master stack and bid clients")
	Require(t, alice.Deposit(ctx, big.NewInt(30)))
	Require(t, bob.Deposit(ctx, big.NewInt(30)))

	// Wait until the next timeboost round + a few milliseconds.
	t.Logf("Alice and Bob are now deposited into the autonomous auction contract, waiting %v for bidding round..., timestamp %v", waitTime, time.Now())
	rawRoundTimingInfo, err := auctionContract.RoundTimingInfo(&bind.CallOpts{})
	Require(t, err)
	roundTimingInfo, err := timeboost.NewRoundTimingInfo(rawRoundTimingInfo)
	Require(t, err)
	time.Sleep(roundTimingInfo.TimeTilNextRound())
	t.Logf("Reached the bidding round at %v", time.Now())
	time.Sleep(time.Second * 5)
	return seqNode, seqClient, seqInfo, proxyAddr, alice, bob, roundDuration, cleanupSeq, builderFeedListener.L2, cleanupFeedListener
}

func awaitAuctionResolved(
	t *testing.T,
	ctx context.Context,
	client *ethclient.Client,
	contract *express_lane_auctiongen.ExpressLaneAuction,
) (common.Address, uint64) {
	fromBlock, err := client.BlockNumber(ctx)
	Require(t, err)
	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return common.Address{}, 0
		case <-ticker.C:
			latestBlock, err := client.HeaderByNumber(ctx, nil)
			if err != nil {
				t.Log("Could not get latest header", err)
				continue
			}
			toBlock := latestBlock.Number.Uint64()
			if fromBlock == toBlock {
				continue
			}
			filterOpts := &bind.FilterOpts{
				Context: ctx,
				Start:   fromBlock,
				End:     &toBlock,
			}
			it, err := contract.FilterAuctionResolved(filterOpts, nil, nil, nil)
			if err != nil {
				t.Log("Could not filter auction resolutions", err)
				continue
			}
			for it.Next() {
				return it.Event.FirstPriceBidder, it.Event.Round
			}
			fromBlock = toBlock
		}
	}
}

type expressLaneClient struct {
	stopwaiter.StopWaiter
	sync.Mutex
	privKey             *ecdsa.PrivateKey
	chainId             *big.Int
	roundTimingInfo     timeboost.RoundTimingInfo
	auctionContractAddr common.Address
	client              *rpc.Client
	sequence            uint64
}

func newExpressLaneClient(
	privKey *ecdsa.PrivateKey,
	chainId *big.Int,
	roundTimingInfo timeboost.RoundTimingInfo,
	auctionContractAddr common.Address,
	client *rpc.Client,
) *expressLaneClient {
	return &expressLaneClient{
		privKey:             privKey,
		chainId:             chainId,
		roundTimingInfo:     roundTimingInfo,
		auctionContractAddr: auctionContractAddr,
		client:              client,
		sequence:            0,
	}
}

func (elc *expressLaneClient) Start(ctxIn context.Context) {
	elc.StopWaiter.Start(ctxIn, elc)
}

func (elc *expressLaneClient) SendTransaction(ctx context.Context, transaction *types.Transaction) error {
	elc.Lock()
	defer elc.Unlock()
	encodedTx, err := transaction.MarshalBinary()
	if err != nil {
		return err
	}
	msg := &timeboost.JsonExpressLaneSubmission{
		ChainId:                (*hexutil.Big)(elc.chainId),
		Round:                  hexutil.Uint64(elc.roundTimingInfo.RoundNumber()),
		AuctionContractAddress: elc.auctionContractAddr,
		Transaction:            encodedTx,
		SequenceNumber:         hexutil.Uint64(elc.sequence),
		Signature:              hexutil.Bytes{},
	}
	msgGo, err := timeboost.JsonSubmissionToGo(msg)
	if err != nil {
		return err
	}
	signingMsg, err := msgGo.ToMessageBytes()
	if err != nil {
		return err
	}
	signature, err := signSubmission(signingMsg, elc.privKey)
	if err != nil {
		return err
	}
	msg.Signature = signature
	promise := elc.sendExpressLaneRPC(msg)
	if _, err := promise.Await(ctx); err != nil {
		return err
	}
	elc.sequence += 1
	return nil
}

func (elc *expressLaneClient) sendExpressLaneRPC(msg *timeboost.JsonExpressLaneSubmission) containers.PromiseInterface[struct{}] {
	return stopwaiter.LaunchPromiseThread(elc, func(ctx context.Context) (struct{}, error) {
		err := elc.client.CallContext(ctx, nil, "timeboost_sendExpressLaneTransaction", msg)
		return struct{}{}, err
	})
}

func signSubmission(message []byte, key *ecdsa.PrivateKey) ([]byte, error) {
	prefixed := crypto.Keccak256(append([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))), message...))
	sig, err := secp256k1.Sign(prefixed, math.PaddedBigBytes(key.D, 32))
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

func getRandomPort(t testing.TB) int {
	listener, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer listener.Close()
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("failed to cast listener address to *net.TCPAddr")
	}
	return tcpAddr.Port
}
