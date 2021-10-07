package arbos

import (
	"bytes"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"math/big"
)

type ArbosAPIImpl struct {
	state        *ArbosState
	currentBlock *blockInProgress
	currentTx    *txInProgress
	coinbaseAddr common.Address
	precompiles  map[common.Address]ArbosPrecompile
}

func NewArbosAPIImpl(backingStorage BackingEvmStorage) *ArbosAPIImpl {
	return &ArbosAPIImpl{
		OpenArbosState(backingStorage),
		nil,
		nil,
		common.BytesToAddress(crypto.Keccak256Hash([]byte("Arbitrum coinbase address")).Bytes()[:20]),
		make(map[common.Address]ArbosPrecompile),
	}
}

func (impl *ArbosAPIImpl) SplitInboxMessage(inputBytes []byte) ([]MessageSegment, error) {
	return ParseIncomingL1Message(bytes.NewReader(inputBytes), impl)
}

func (impl *ArbosAPIImpl) FinalizeBlock(header *types.Header, stateDB *state.StateDB, txs types.Transactions) {
	// process deposit, if there is one
	deposit := impl.currentBlock.depositSegmentRemaining
	if deposit != nil {
		stateDB.AddBalance(deposit.addr, deposit.balance.Big())
	}

	// reimburse aggregators from the coinbase address
	coinbaseWei := stateDB.GetBalance(impl.coinbaseAddr)
	for agg, amount := range impl.currentBlock.weiOwedToAggregators {
		if amount.Cmp(coinbaseWei) <= 0 {
			coinbaseWei = new(big.Int).Sub(coinbaseWei, amount)
			stateDB.AddBalance(agg, amount)
		}
	}
	stateDB.SetBalance(impl.coinbaseAddr, coinbaseWei)

	impl.state.backingStorage.Flush()
}

func (impl *ArbosAPIImpl) StartTxHook(msg core.Message, state vm.StateDB) (uint64, error) {  // uint64 return is extra gas to charge
	impl.currentTx = newTxInProgress()
	extraGasChargeWei, aggregator := impl.currentTx.getExtraGasChargeWei()
	gasPrice := msg.GasPrice()
	extraGas := new(big.Int).Div(extraGasChargeWei, gasPrice)
	var extraGasI64 int64
	if extraGas.IsInt64() {
		extraGasI64 = extraGas.Int64()
	} else {
		extraGasI64 = math.MaxInt64
	}
	extraGasChargeWei = new(big.Int).Mul(gasPrice, big.NewInt(extraGasI64))
	if aggregator != nil {
		impl.currentBlock.creditAggregator(*aggregator, extraGasChargeWei)
	}
	return uint64(extraGasI64), nil
}

func (impl *ArbosAPIImpl) EndTxHook(
	msg core.Message,
	totalGasUsed uint64,
	extraGasCharged uint64,
	state vm.StateDB,
) error {
	return nil
}

func (impl *ArbosAPIImpl) GetExtraSegmentToBeNextBlock() *MessageSegment {
	return nil
}

func (impl *ArbosAPIImpl) Precompiles() map[common.Address]ArbosPrecompile {
	return impl.precompiles
}

type ethDeposit struct {
	api     *ArbosAPIImpl
	addr    common.Address
	balance common.Hash
}

func (deposit *ethDeposit) CreateBlockContents(
	beforeState *state.StateDB,
) (
	[]*types.Transaction, // transactions to (try to) put in the block
	*big.Int,             // timestamp
	common.Address,       // coinbase address
	error,
) {
	deposit.api.currentBlock = newBlockInProgress(nil, deposit)
	return []*types.Transaction{}, deposit.api.state.LastTimestampSeen().Big(), deposit.api.coinbaseAddr, nil
}

type txSegment struct {
	api         *ArbosAPIImpl
	tx          *types.Transaction
}

func (seg *txSegment) CreateBlockContents(
	beforeState *state.StateDB,
) (
	[]*types.Transaction, // transactions to (try to) put in the block
	*big.Int,             // timestamp
	common.Address,       // coinbase address
	error,
) {
	seg.api.currentBlock = newBlockInProgress(seg, nil)
	return []*types.Transaction{ seg.tx }, seg.api.state.LastTimestampSeen().Big(), seg.api.coinbaseAddr, nil
}

type blockInProgress struct {
	txSegmentRemaining      MessageSegment
	depositSegmentRemaining *ethDeposit
	weiOwedToAggregators map[common.Address]*big.Int
}

func newBlockInProgress(seg MessageSegment, deposit *ethDeposit) *blockInProgress {
	return &blockInProgress{
		seg,
		deposit,
		make(map[common.Address]*big.Int),
	}
}

func (bip *blockInProgress) creditAggregator(agg common.Address, wei *big.Int) {
	old, exists := bip.weiOwedToAggregators[agg]
	if !exists {
		old = big.NewInt(0)
	}
	bip.weiOwedToAggregators[agg] = new(big.Int).Add(old, wei)
}

type txInProgress struct {
}

func newTxInProgress() *txInProgress {
	return &txInProgress{}
}

func (tx *txInProgress) getExtraGasChargeWei() (*big.Int, *common.Address) {  // returns wei to charge, address to give it to
	//TODO
	return big.NewInt(0), nil
}

// Implementation of Transaction for txSegment

func (seg *txSegment) txType() byte                          { return seg.tx.Type() }
func (seg *txSegment) chainID() *big.Int                     { return seg.tx.ChainId() }
func (seg *txSegment) accessList() types.AccessList          { return seg.tx.AccessList() }
func (seg *txSegment) data() []byte                          { return seg.tx.Data() }
func (seg *txSegment) gas() uint64                           { return seg.tx.Gas() }
func (seg *txSegment) gasPrice() *big.Int                    { return seg.tx.GasPrice() }
func (seg *txSegment) gasTipCap() *big.Int                   { return seg.tx.GasTipCap() }
func (seg *txSegment) gasFeeCap() *big.Int                   { return seg.tx.GasFeeCap() }
func (seg *txSegment) value() *big.Int                       { return seg.tx.Value() }
func (seg *txSegment) nonce() uint64                         { return seg.tx.Nonce() }
func (seg *txSegment) to() *common.Address                   { return seg.tx.To() }
func (seg txSegment) rawSignatureValues() (v, r, s *big.Int) { return seg.tx.RawSignatureValues() }
