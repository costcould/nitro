// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package validator

import (
	"context"
	"fmt"
	"sync"

	"github.com/offchainlabs/nitro/arbutil"

	"github.com/ethereum/go-ethereum/arbitrum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbosState"
	"github.com/offchainlabs/nitro/arbstate"
	"github.com/pkg/errors"
)

type StatelessBlockValidator struct {
	MachineLoader   *NitroMachineLoader
	inboxReader     InboxReaderInterface
	inboxTracker    InboxTrackerInterface
	streamer        TransactionStreamerInterface
	blockchain      *core.BlockChain
	db              ethdb.Database
	daService       arbstate.DataAvailabilityReader
	genesisBlockNum uint64
	stateDatabase   state.Database

	moduleMutex           sync.Mutex
	currentWasmModuleRoot common.Hash
	pendingWasmModuleRoot common.Hash
	fatalErrChan          chan error
}

type BlockValidatorRegistrer interface {
	SetBlockValidator(*BlockValidator)
}

type InboxTrackerInterface interface {
	BlockValidatorRegistrer
	GetDelayedMessageBytes(uint64) ([]byte, error)
	GetBatchMessageCount(seqNum uint64) (arbutil.MessageIndex, error)
	GetBatchAcc(seqNum uint64) (common.Hash, error)
	GetBatchCount() (uint64, error)
}

type TransactionStreamerInterface interface {
	BlockValidatorRegistrer
	GetMessage(seqNum arbutil.MessageIndex) (*arbstate.MessageWithMetadata, error)
	GetGenesisBlockNumber() (uint64, error)
	PauseReorgs()
	ResumeReorgs()
}

type InboxReaderInterface interface {
	GetSequencerMessageBytes(ctx context.Context, seqNum uint64) ([]byte, error)
}

type L1ReaderInterface interface {
	Client() arbutil.L1Interface
	Subscribe(bool) (<-chan *types.Header, func())
	WaitForTxApproval(ctx context.Context, tx *types.Transaction) (*types.Receipt, error)
}

type GlobalStatePosition struct {
	BatchNumber uint64
	PosInBatch  uint64
}

func GlobalStatePositionsFor(
	tracker InboxTrackerInterface,
	pos arbutil.MessageIndex,
	batch uint64,
) (GlobalStatePosition, GlobalStatePosition, error) {
	msgCountInBatch, err := tracker.GetBatchMessageCount(batch)
	if err != nil {
		return GlobalStatePosition{}, GlobalStatePosition{}, err
	}
	var firstInBatch arbutil.MessageIndex
	if batch > 0 {
		firstInBatch, err = tracker.GetBatchMessageCount(batch - 1)
		if err != nil {
			return GlobalStatePosition{}, GlobalStatePosition{}, err
		}
	}
	if msgCountInBatch <= pos {
		return GlobalStatePosition{}, GlobalStatePosition{}, fmt.Errorf("batch %d has up to message %d, failed getting for %d", batch, msgCountInBatch-1, pos)
	}
	if firstInBatch > pos {
		return GlobalStatePosition{}, GlobalStatePosition{}, fmt.Errorf("batch %d starts from %d, failed getting for %d", batch, firstInBatch, pos)
	}
	startPos := GlobalStatePosition{batch, uint64(pos - firstInBatch)}
	if msgCountInBatch == pos+1 {
		return startPos, GlobalStatePosition{batch + 1, 0}, nil
	}
	return startPos, GlobalStatePosition{batch, uint64(pos + 1 - firstInBatch)}, nil
}

func FindBatchContainingMessageIndex(
	tracker InboxTrackerInterface, pos arbutil.MessageIndex, high uint64,
) (uint64, error) {
	var low uint64
	// Iteration preconditions:
	// - high >= low
	// - msgCount(low - 1) <= pos implies low <= target
	// - msgCount(high) > pos implies high >= target
	// Therefore, if low == high, then low == high == target
	for high > low {
		// Due to integer rounding, mid >= low && mid < high
		mid := (low + high) / 2
		count, err := tracker.GetBatchMessageCount(mid)
		if err != nil {
			return 0, err
		}
		if count < pos {
			// Must narrow as mid >= low, therefore mid + 1 > low, therefore newLow > oldLow
			// Keeps low precondition as msgCount(mid) < pos
			low = mid + 1
		} else if count == pos {
			return mid + 1, nil
		} else if count == pos+1 || mid == low { // implied: count > pos
			return mid, nil
		} else { // implied: count > pos + 1
			// Must narrow as mid < high, therefore newHigh < lowHigh
			// Keeps high precondition as msgCount(mid) > pos
			high = mid
		}
	}
	return low, nil
}

type ValidationEntryStage uint32

const (
	Empty ValidationEntryStage = iota
	ReadyForRecord
	Recorded
	Ready
)

type validationEntry struct {
	Stage ValidationEntryStage
	// Valid since ReadyforRecord:
	BlockNumber     uint64
	PrevBlockHash   common.Hash
	PrevBlockHeader *types.Header
	BlockHash       common.Hash
	BlockHeader     *types.Header
	HasDelayedMsg   bool
	DelayedMsgNr    uint64
	msg             *arbstate.MessageWithMetadata
	// Valid since Recorded:
	Preimages map[common.Hash][]byte
	BatchInfo []BatchInfo
	// Valid since Ready:
	StartPosition GlobalStatePosition
	EndPosition   GlobalStatePosition
}

func (v *validationEntry) start() (GoGlobalState, error) {
	start := v.StartPosition
	prevExtraInfo, err := types.DeserializeHeaderExtraInformation(v.PrevBlockHeader)
	if err != nil {
		return GoGlobalState{}, err
	}
	return GoGlobalState{
		Batch:      start.BatchNumber,
		PosInBatch: start.PosInBatch,
		BlockHash:  v.PrevBlockHash,
		SendRoot:   prevExtraInfo.SendRoot,
	}, nil
}

func (v *validationEntry) expectedEnd() (GoGlobalState, error) {
	extraInfo, err := types.DeserializeHeaderExtraInformation(v.BlockHeader)
	if err != nil {
		return GoGlobalState{}, err
	}
	end := v.EndPosition
	return GoGlobalState{
		Batch:      end.BatchNumber,
		PosInBatch: end.PosInBatch,
		BlockHash:  v.BlockHash,
		SendRoot:   extraInfo.SendRoot,
	}, nil
}

func newValidationEntry(
	prevHeader *types.Header,
	header *types.Header,
	msg *arbstate.MessageWithMetadata,
) (*validationEntry, error) {
	var hasDelayedMsg bool
	var delayedMsgNr uint64
	if prevHeader == nil || header.Nonce != prevHeader.Nonce {
		hasDelayedMsg = true
		if prevHeader != nil {
			delayedMsgNr = prevHeader.Nonce.Uint64()
		}
	}
	return &validationEntry{
		Stage:           ReadyForRecord,
		BlockNumber:     header.Number.Uint64(),
		PrevBlockHash:   prevHeader.Hash(),
		PrevBlockHeader: prevHeader,
		BlockHash:       header.Hash(),
		BlockHeader:     header,
		HasDelayedMsg:   hasDelayedMsg,
		DelayedMsgNr:    delayedMsgNr,
		msg:             msg,
	}, nil
}

func newRecordedValidationEntry(
	prevHeader *types.Header,
	header *types.Header,
	preimages map[common.Hash][]byte,
	batchInfos []BatchInfo,
) (*validationEntry, error) {
	entry, err := newValidationEntry(prevHeader, header, nil)
	if err != nil {
		return nil, err
	}
	entry.Preimages = preimages
	entry.BatchInfo = batchInfos
	entry.Stage = Recorded
	return entry, nil
}

func NewStatelessBlockValidator(
	machineLoader *NitroMachineLoader,
	inboxReader InboxReaderInterface,
	inbox InboxTrackerInterface,
	streamer TransactionStreamerInterface,
	blockchain *core.BlockChain,
	blockchainDb ethdb.Database,
	arbdb ethdb.Database,
	das arbstate.DataAvailabilityReader,
	config *BlockValidatorConfig,
	fatalErrChan chan error,
) (*StatelessBlockValidator, error) {
	genesisBlockNum, err := streamer.GetGenesisBlockNumber()
	if err != nil {
		return nil, err
	}
	validator := &StatelessBlockValidator{
		MachineLoader:   machineLoader,
		inboxReader:     inboxReader,
		inboxTracker:    inbox,
		streamer:        streamer,
		blockchain:      blockchain,
		db:              arbdb,
		daService:       das,
		genesisBlockNum: genesisBlockNum,
		stateDatabase:   state.NewDatabaseWithConfig(blockchainDb, &trie.Config{Cache: 16}), // TODO: configurable cache size
		fatalErrChan:    fatalErrChan,
	}
	if config.PendingUpgradeModuleRoot != "" {
		if config.PendingUpgradeModuleRoot == "latest" {
			latest, err := machineLoader.GetConfig().ReadLatestWasmModuleRoot()
			if err != nil {
				return nil, err
			}
			validator.pendingWasmModuleRoot = latest
		} else {
			validator.pendingWasmModuleRoot = common.HexToHash(config.PendingUpgradeModuleRoot)
			if (validator.pendingWasmModuleRoot == common.Hash{}) {
				return nil, errors.New("pending-upgrade-module-root config value illegal")
			}
		}

		// the machine will be lazily created if need be later otherwise
		if config.ArbitratorValidator {
			if err := machineLoader.CreateMachine(validator.pendingWasmModuleRoot, true, false); err != nil {
				return nil, err
			}
		}
		if config.JitValidator {
			if err := machineLoader.CreateMachine(validator.pendingWasmModuleRoot, true, true); err != nil {
				return nil, err
			}
		}
	}
	return validator, nil
}

func (v *StatelessBlockValidator) GetModuleRootsToValidate() []common.Hash {
	v.moduleMutex.Lock()
	defer v.moduleMutex.Unlock()

	validatingModuleRoots := []common.Hash{v.currentWasmModuleRoot}
	if (v.currentWasmModuleRoot != v.pendingWasmModuleRoot && v.pendingWasmModuleRoot != common.Hash{}) {
		validatingModuleRoots = append(validatingModuleRoots, v.pendingWasmModuleRoot)
	}
	return validatingModuleRoots
}

type BatchInfo struct {
	Number uint64
	Data   []byte
}

// If msg is nil, this will record block creation up to the point where message would be accessed (for a "too far" proof)
func RecordBlockCreation(
	ctx context.Context,
	blockchain *core.BlockChain,
	stateDatabase state.Database,
	inboxReader InboxReaderInterface,
	prevHeader *types.Header,
	msg *arbstate.MessageWithMetadata,
	producePreimages bool,
) (common.Hash, map[common.Hash][]byte, []BatchInfo, error) {
	var recordingdb *state.StateDB
	var chaincontext core.ChainContext
	var recordingKV *arbitrum.RecordingKV
	var err error
	if prevHeader != nil {
		// make sure blockchain has the required state
		_, err = arbitrum.GetOrRecreateReferencedState(ctx, prevHeader, blockchain, stateDatabase)
		if err != nil {
			return common.Hash{}, nil, nil, err
		}
		defer arbitrum.DereferenceState(prevHeader, stateDatabase)
	}
	if producePreimages {
		recordingdb, chaincontext, recordingKV, err = arbitrum.PrepareRecording(stateDatabase.TrieDB(), blockchain, prevHeader)
		if err != nil {
			return common.Hash{}, nil, nil, err
		}
	} else {
		var prevRoot common.Hash
		if prevHeader != nil {
			prevRoot = prevHeader.Root
		}
		recordingdb, err = blockchain.StateAt(prevRoot)
		if err != nil {
			return common.Hash{}, nil, nil, err
		}
		chaincontext = blockchain
	}

	chainConfig := blockchain.Config()

	// Get the chain ID, both to validate and because the replay binary also gets the chain ID,
	// so we need to populate the recordingdb with preimages for retrieving the chain ID.
	if prevHeader != nil {
		initialArbosState, err := arbosState.OpenSystemArbosState(recordingdb, nil, true)
		if err != nil {
			return common.Hash{}, nil, nil, fmt.Errorf("error opening initial ArbOS state: %w", err)
		}
		chainId, err := initialArbosState.ChainId()
		if err != nil {
			return common.Hash{}, nil, nil, fmt.Errorf("error getting chain ID from initial ArbOS state: %w", err)
		}
		if chainId.Cmp(chainConfig.ChainID) != 0 {
			return common.Hash{}, nil, nil, fmt.Errorf("unexpected chain ID %v in ArbOS state, expected %v", chainId, chainConfig.ChainID)
		}
		genesisNum, err := initialArbosState.GenesisBlockNum()
		if err != nil {
			return common.Hash{}, nil, nil, fmt.Errorf("error getting genesis block number from initial ArbOS state: %w", err)
		}
		expectedNum := chainConfig.ArbitrumChainParams.GenesisBlockNum
		if genesisNum != expectedNum {
			return common.Hash{}, nil, nil, fmt.Errorf("unexpected genesis block number %v in ArbOS state, expected %v", genesisNum, expectedNum)
		}
	}

	var blockHash common.Hash
	var readBatchInfo []BatchInfo
	if msg != nil {
		batchFetcher := func(batchNum uint64) ([]byte, error) {
			data, err := inboxReader.GetSequencerMessageBytes(ctx, batchNum)
			if err != nil {
				return nil, err
			}
			readBatchInfo = append(readBatchInfo, BatchInfo{
				Number: batchNum,
				Data:   data,
			})
			return data, nil
		}
		block, _, err := arbos.ProduceBlock(
			msg.Message,
			msg.DelayedMessagesRead,
			prevHeader,
			recordingdb,
			chaincontext,
			chainConfig,
			batchFetcher,
		)
		if err != nil {
			return common.Hash{}, nil, nil, err
		}
		blockHash = block.Hash()
	}

	var preimages map[common.Hash][]byte
	if recordingKV != nil {
		preimages, err = arbitrum.PreimagesFromRecording(chaincontext, recordingKV)
		if err != nil {
			return common.Hash{}, nil, nil, err
		}
	}
	return blockHash, preimages, readBatchInfo, err
}

func BlockDataForValidation(
	ctx context.Context,
	blockchain *core.BlockChain,
	stateDatabase state.Database,
	inboxReader InboxReaderInterface,
	header, prevHeader *types.Header,
	msg arbstate.MessageWithMetadata,
	das arbstate.DataAvailabilityReader,
	producePreimages bool,
) (
	preimages map[common.Hash][]byte, readBatchInfo []BatchInfo,
	hasDelayedMessage bool, delayedMsgNr uint64, err error,
) {
	var prevHash common.Hash
	if prevHeader != nil {
		prevHash = prevHeader.Hash()
	}
	if header.ParentHash != prevHash {
		err = fmt.Errorf("bad arguments: prev does not match")
		return
	}

	if prevHeader != nil {
		var blockhash common.Hash
		blockhash, preimages, readBatchInfo, err = RecordBlockCreation(
			ctx, blockchain, stateDatabase, inboxReader, prevHeader, &msg, producePreimages,
		)
		if err != nil {
			return
		}
		if blockhash != header.Hash() {
			err = fmt.Errorf("wrong hash expected %s got %s", header.Hash(), blockhash)
			return
		}
	}

	if prevHeader == nil || header.Nonce != prevHeader.Nonce {
		hasDelayedMessage = true
		if prevHeader != nil {
			delayedMsgNr = prevHeader.Nonce.Uint64()
		}
	}

	return
}

func ValidationEntryRecord(ctx context.Context, e *validationEntry,
	blockchain *core.BlockChain, stateDatabase state.Database, inboxReader InboxReaderInterface, das arbstate.DataAvailabilityReader,
	producePreimages bool) error {
	if e.Stage != ReadyForRecord {
		return errors.Errorf("validation entry should be ReadyForRecord, is: %v", e.Stage)
	}
	if e.PrevBlockHeader == nil {
		e.Stage = Recorded
		return nil
	}
	blockhash, preimages, readBatchInfo, err := RecordBlockCreation(
		ctx, blockchain, stateDatabase, inboxReader, e.PrevBlockHeader, e.msg, producePreimages,
	)
	if err != nil {
		return err
	}
	if blockhash != e.BlockHash {
		return fmt.Errorf("recording failed: blockNum %d, hash expected %v, got %v", e.BlockNumber, e.BlockHash, blockhash)
	}
	e.Preimages = preimages
	e.BatchInfo = readBatchInfo
	e.msg = nil // no longer needed
	e.Stage = Recorded
	return nil
}

func AddPreimagesFromBatchInfos(
	ctx context.Context,
	preimages map[common.Hash][]byte,
	batchInfos []BatchInfo,
	blockchain *core.BlockChain,
	das arbstate.DataAvailabilityReader,
) error {

	for _, batch := range batchInfos {
		if len(batch.Data) <= 40 {
			continue
		}
		if !arbstate.IsDASMessageHeaderByte(batch.Data[40]) {
			continue
		}
		if das == nil {
			log.Error("No DAS configured, but sequencer message found with DAS header")
			if blockchain.Config().ArbitrumChainParams.DataAvailabilityCommittee {
				return errors.New("processing data availability chain without DAS configured")
			}
		} else {
			_, err := arbstate.RecoverPayloadFromDasBatch(
				ctx, batch.Number, batch.Data, das, preimages, arbstate.KeysetValidate,
			)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidationEntryAddSeqMessage(ctx context.Context, e *validationEntry,
	startPos, endPos GlobalStatePosition, seqMsg []byte,
	blockchain *core.BlockChain, das arbstate.DataAvailabilityReader) error {
	if e.Stage != Recorded {
		return fmt.Errorf("validation entry stage should be Recorded, is: %v", e.Stage)
	}
	if e.Preimages == nil {
		e.Preimages = make(map[common.Hash][]byte)
	}
	if e.BatchInfo == nil {
		e.BatchInfo = make([]BatchInfo, 0, 1)
	}
	e.StartPosition = startPos
	e.EndPosition = endPos
	seqMsgBatchInfo := BatchInfo{
		Number: startPos.BatchNumber,
		Data:   seqMsg,
	}
	e.BatchInfo = append(e.BatchInfo, seqMsgBatchInfo)
	err := AddPreimagesFromBatchInfos(ctx, e.Preimages, e.BatchInfo, blockchain, das)
	if err != nil {
		return err
	}
	e.Stage = Ready
	return nil
}

func NewMachinePreimageResolver(
	ctx context.Context,
	preimages map[common.Hash][]byte,
	bc *core.BlockChain,
) (GoPreimageResolver, error) {
	recordNewPreimages := true
	if preimages == nil {
		preimages = make(map[common.Hash][]byte)
		recordNewPreimages = false
	}

	db := bc.StateCache().TrieDB()
	resolver := func(hash common.Hash) ([]byte, error) {
		// Check if it's a known preimage
		if preimage, ok := preimages[hash]; ok {
			return preimage, nil
		}
		// Check if it's part of the state trie
		preimage, err := db.Node(hash)
		if err != nil {
			// Check if it's a code hash
			codeKey := append([]byte{}, rawdb.CodePrefix...)
			codeKey = append(codeKey, hash.Bytes()...)
			preimage, err = db.DiskDB().Get(codeKey)
		}
		if err != nil {
			// Check if it's a block hash
			header := bc.GetHeaderByHash(hash)
			if header != nil {
				preimage, err = rlp.EncodeToBytes(header)
			}
		}
		if err == nil && recordNewPreimages {
			preimages[hash] = preimage
		}
		return preimage, err
	}
	return resolver, nil
}

func (v *StatelessBlockValidator) executeBlock(
	ctx context.Context, entry *validationEntry, moduleRoot common.Hash,
) (GoGlobalState, []byte, error) {
	start := entry.StartPosition
	gsStart, err := entry.start()
	if err != nil {
		return GoGlobalState{}, nil, err
	}
	basemachine, err := v.MachineLoader.GetMachine(ctx, moduleRoot, true)
	if err != nil {
		return GoGlobalState{}, nil, fmt.Errorf("unabled to get WASM machine: %w", err)
	}
	mach := basemachine.Clone()
	resolver, err := NewMachinePreimageResolver(ctx, entry.Preimages, v.blockchain)
	if err != nil {
		return GoGlobalState{}, nil, err
	}
	if err := mach.SetPreimageResolver(resolver); err != nil {
		return GoGlobalState{}, nil, err
	}
	err = mach.SetGlobalState(gsStart)
	if err != nil {
		log.Error("error while setting global state for proving", "err", err, "gsStart", gsStart)
		return GoGlobalState{}, nil, errors.New("error while setting global state for proving")
	}
	for _, batch := range entry.BatchInfo {
		err = mach.AddSequencerInboxMessage(batch.Number, batch.Data)
		if err != nil {
			log.Error(
				"error while trying to add sequencer msg for proving",
				"err", err, "seq", start.BatchNumber, "blockNr", entry.BlockNumber,
			)
			return GoGlobalState{}, nil, errors.New("error while trying to add sequencer msg for proving")
		}
	}
	var delayedMsg []byte
	if entry.HasDelayedMsg {
		delayedMsg, err = v.inboxTracker.GetDelayedMessageBytes(entry.DelayedMsgNr)
		if err != nil {
			log.Error(
				"error while trying to read delayed msg for proving",
				"err", err, "seq", entry.DelayedMsgNr, "blockNr", entry.BlockNumber,
			)
			return GoGlobalState{}, nil, errors.New("error while trying to read delayed msg for proving")
		}
		err = mach.AddDelayedInboxMessage(entry.DelayedMsgNr, delayedMsg)
		if err != nil {
			log.Error(
				"error while trying to add delayed msg for proving",
				"err", err, "seq", entry.DelayedMsgNr, "blockNr", entry.BlockNumber,
			)
			return GoGlobalState{}, nil, errors.New("error while trying to add delayed msg for proving")
		}
	}

	var steps uint64
	for mach.IsRunning() {
		var count uint64 = 500000000
		err = mach.Step(ctx, count)
		if steps > 0 {
			log.Debug("validation", "moduleRoot", moduleRoot, "block", entry.BlockNumber, "steps", steps)
		}
		if err != nil {
			return GoGlobalState{}, nil, fmt.Errorf("machine execution failed with error: %w", err)
		}
		steps += count
	}
	if mach.IsErrored() {
		log.Error("machine entered errored state during attempted validation", "block", entry.BlockNumber)
		return GoGlobalState{}, nil, errors.New("machine entered errored state during attempted validation")
	}
	return mach.GetGlobalState(), delayedMsg, nil
}

func (v *StatelessBlockValidator) jitBlock(
	ctx context.Context, entry *validationEntry, moduleRoot common.Hash,
) (GoGlobalState, []byte, error) {
	empty := GoGlobalState{}

	machine, err := v.MachineLoader.GetJitMachine(ctx, moduleRoot, true)
	if err != nil {
		return empty, nil, fmt.Errorf("unabled to get WASM machine: %w", err)
	}

	var delayed []byte
	if entry.HasDelayedMsg {
		delayed, err = v.inboxTracker.GetDelayedMessageBytes(entry.DelayedMsgNr)
		if err != nil {
			log.Error(
				"error while trying to read delayed msg for jitting",
				"err", err, "seq", entry.DelayedMsgNr, "blockNr", entry.BlockNumber,
			)
			return empty, nil, errors.New("error while trying to read delayed msg for proving")
		}
	}

	resolver, err := NewMachinePreimageResolver(ctx, entry.Preimages, v.blockchain)
	if err != nil {
		return empty, nil, err
	}
	state, err := machine.prove(ctx, entry, resolver, delayed)
	return state, delayed, err
}

func (v *StatelessBlockValidator) ValidateBlock(
	ctx context.Context, header *types.Header, full bool, moduleRoot common.Hash,
) (bool, error) {
	if header == nil {
		return false, errors.New("header not found")
	}
	blockNum := header.Number.Uint64()
	msgIndex := arbutil.BlockNumberToMessageCount(blockNum, v.genesisBlockNum) - 1
	prevHeader := v.blockchain.GetHeaderByNumber(blockNum - 1)
	if prevHeader == nil {
		return false, errors.New("prev header not found")
	}
	msg, err := v.streamer.GetMessage(msgIndex)
	if err != nil {
		return false, err
	}
	preimages, readBatchInfo, _, _, err := BlockDataForValidation(
		ctx, v.blockchain, v.stateDatabase, v.inboxReader, header, prevHeader, *msg, v.daService, false,
	)
	if err != nil {
		return false, fmt.Errorf("failed to get block data to validate: %w", err)
	}

	batchCount, err := v.inboxTracker.GetBatchCount()
	if err != nil {
		return false, err
	}
	batch, err := FindBatchContainingMessageIndex(v.inboxTracker, msgIndex, batchCount)
	if err != nil {
		return false, err
	}

	startPos, endPos, err := GlobalStatePositionsFor(v.inboxTracker, msgIndex, batch)
	if err != nil {
		return false, fmt.Errorf("failed calculating position for validation: %w", err)
	}

	entry, err := newRecordedValidationEntry(
		prevHeader, header, preimages, readBatchInfo,
	)
	if err != nil {
		return false, fmt.Errorf("failed to create validation entry %w", err)
	}

	seqMsg, err := v.inboxReader.GetSequencerMessageBytes(ctx, startPos.BatchNumber)
	if err != nil {
		return false, err
	}
	err = ValidationEntryAddSeqMessage(ctx, entry, startPos, endPos, seqMsg, v.blockchain, v.daService)
	if err != nil {
		return false, err
	}
	expEnd, err := entry.expectedEnd()
	if err != nil {
		return false, err
	}
	var gsEnd GoGlobalState
	if full {
		gsEnd, _, err = v.executeBlock(ctx, entry, moduleRoot)
	} else {
		gsEnd, _, err = v.jitBlock(ctx, entry, moduleRoot)
	}
	if err != nil {
		return false, err
	}
	return gsEnd == expEnd, nil
}
