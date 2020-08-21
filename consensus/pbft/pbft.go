package pbft

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	"github.com/elastos/Elastos.ELA.SideChain.ETH/common"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/consensus"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/core"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/core/state"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/core/types"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/dpos"
	emsg "github.com/elastos/Elastos.ELA.SideChain.ETH/dpos/msg"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/log"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/params"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/rlp"
	"github.com/elastos/Elastos.ELA.SideChain.ETH/rpc"
	"github.com/elastos/Elastos.ELA/core/types/payload"

	ecom "github.com/elastos/Elastos.ELA/common"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	dmsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/events"
	"github.com/elastos/Elastos.ELA/p2p/msg"

	"golang.org/x/crypto/sha3"
)

var (
	extraVanity = 32 // Fixed number of extra-data prefix bytes
	extraSeal   = 65 // Fixed number of extra-data suffix bytes reserved for signer seal

)

const (
	// maxRequestedBlocks is the maximum number of requested block
	// hashes to store in memory.
	maxRequestedBlocks = msg.MaxInvPerMsg
)

var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")
	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")
	// to contain a 64 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 64 byte signature suffix missing")
	// errUnauthorizedSigner is returned if a header is signed by a non-authorized entity.
	errUnauthorizedSigner = errors.New("unauthorized signer")
	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	ErrAlreadyConfirmedBlock = errors.New("already confirmed block")

	ErrWaitSyncBlock = errors.New("has confirmed, wait sync block")

	ErrInvalidConfirm = errors.New("invalid confirm")
)

// Pbft is a consensus engine based on Byzantine fault-tolerant algorithm
type Pbft struct {
	datadir     string
	cfg         params.PbftConfig
	dispatcher  *dpos.Dispatcher
	confirmCh   chan *payload.Confirm
	unConfirmCh chan *payload.Confirm
	account     daccount.Account
	network     *dpos.Network
	blockPool   *dpos.BlockPool
	chain       *core.BlockChain
	timeSource  dtime.MedianTimeSource

	// IsCurrent returns whether BlockChain synced to best height.
	IsCurrent func() bool
	StartMine func()

	requestedBlocks    map[common.Hash]struct{}
	requestedProposals map[ecom.Uint256]struct{}
	statusMap          map[uint32]map[string]*dmsg.ConsensusStatus
	notHandledProposal map[string]struct{}

	enableViewLoop bool
	recoverStarted bool
	isRecoved      bool
	period         uint64
	isSealOver     bool
	isRecovering   bool
}

func New(cfg *params.PbftConfig, pbftKeystore string, password []byte, dataDir string) *Pbft {
	logpath := filepath.Join(dataDir, "/logs/dpos")
	dposPath := filepath.Join(dataDir, "/network/dpos")
	if strings.LastIndex(dataDir, "/") == len(dataDir)-1 {
		dposPath = filepath.Join(dataDir, "network/dpos")
		logpath = filepath.Join(dataDir, "logs/dpos")
	}
	if cfg == nil {
		dpos.InitLog(0, 0, 0, logpath)
		return &Pbft{}
	}
	dpos.InitLog(cfg.PrintLevel, cfg.MaxPerLogSize, cfg.MaxLogsSize, logpath)
	producers := make([][]byte, len(cfg.Producers))
	for i, v := range cfg.Producers {
		producers[i] = common.Hex2Bytes(v)
	}
	account, err := dpos.GetDposAccount(pbftKeystore, password)
	if err != nil {
		fmt.Println("create dpos account error:", err.Error(), "pbftKeystore:", pbftKeystore, "password")
		//can't return, because common node need verify use this engine
	}
	medianTimeSouce := dtime.NewMedianTime()

	pbft := &Pbft{
		datadir:            dataDir,
		cfg:                *cfg,
		confirmCh:          make(chan *payload.Confirm),
		unConfirmCh:		make(chan *payload.Confirm),
		account:            account,
		requestedBlocks:    make(map[common.Hash]struct{}),
		requestedProposals: make(map[ecom.Uint256]struct{}),
		statusMap:          make(map[uint32]map[string]*dmsg.ConsensusStatus),
		notHandledProposal: make(map[string]struct{}),
		period:             5,
		timeSource:         medianTimeSouce,
	}
	blockPool := dpos.NewBlockPool(pbft.verifyConfirm, pbft.verifyBlock, DBlockSealHash)
	pbft.blockPool = blockPool
	var accpubkey []byte

	if account != nil {
		accpubkey = account.PublicKeyBytes()
		network, err := dpos.NewNetwork(&dpos.NetworkConfig{
			IPAddress:   cfg.IPAddress,
			Magic:       cfg.Magic,
			DefaultPort: cfg.DPoSPort,
			Account:     account,
			MedianTime:  medianTimeSouce,
			Listener:    pbft,
			DataPath:    dposPath,
			PublicKey:   accpubkey,
			AnnounceAddr: func() {
				events.Notify(dpos.ETAnnounceAddr, nil)
			},
		})
		if err != nil {
			dpos.Error("New dpos network error:", err.Error())
			return nil
		}
		pbft.network = network
		pbft.subscribeEvent()
	}
	pbft.dispatcher = dpos.NewDispatcher(producers, pbft.onConfirm, pbft.onUnConfirm,
		10*time.Second, accpubkey, medianTimeSouce, pbft)
	return pbft
}

func (p *Pbft) subscribeEvent() {
	events.Subscribe(func(e *events.Event) {
		switch e.Type {
		case events.ETDirectPeersChanged:
			go p.network.UpdatePeers(e.Data.([]peer.PID))
		case dpos.ETNewPeer:
			count := len(p.network.GetActivePeers())
			log.Info("new peer accept","active peer count", count)
			if p.chain.Engine() == p && !p.dispatcher.GetConsensusView().HasProducerMajorityCount(count) {
				go p.AnnounceDAddr()
			}
		}
	})
}

func (p *Pbft) GetDataDir() string {
	return p.datadir
}

func (p *Pbft) GetPbftConfig() params.PbftConfig {
	return p.cfg
}

func (p *Pbft) Author(header *types.Header) (common.Address, error) {
	dpos.Info("Pbft Author")
	// TODO panic("implement me")
	return header.Coinbase, nil
}

func (p *Pbft) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	dpos.Info("Pbft VerifyHeader")
	return p.verifyHeader(chain, header, nil, seal)
}

func (p *Pbft) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	dpos.Info("Pbft VerifyHeaders")
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			var err error
			//Check header is verified
			if seals[i] {
				// Don't waste time checking blocks from the future
				if header.Time > uint64(time.Now().Unix()) {
					err = consensus.ErrFutureBlock
				}
			}
			if err == nil && !p.IsInBlockPool(p.SealHash(header)) {
				err = p.verifyHeader(chain, header, headers[:i], seals[i])
			} else {
				err = p.verifySeal(chain, header, headers[:i])
			}

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

func (p *Pbft) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header, seal bool) error {

	if header.Number == nil || header.Number.Uint64() == 0 {
		return errUnknownBlock
	}
	// Don't waste time checking blocks from the future
	if seal && header.Time > uint64(time.Now().Unix()) {
		return consensus.ErrFutureBlock
	}

	// Verify that the gas limit is <= 2^63-1
	cap := uint64(0x7fffffffffffffff)
	if header.GasLimit > cap {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, cap)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}
	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in Pbft
	if header.UncleHash != types.CalcUncleHash(nil) {
		return errInvalidUncleHash
	}

	number := header.Number.Uint64()
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	log.Info("verify header HashConfirmed", "seal:", seal, "height", header.Number)
	if !seal && p.dispatcher.GetFinishedHeight() == number {
		log.Info("verify header already confirm block")
		return ErrAlreadyConfirmedBlock
	}

	if parent.Time+p.period > header.Time {
		return ErrInvalidTimestamp
	}

	if seal {
		return p.verifySeal(chain, header, parents)
	}

	return nil
}

func (p *Pbft) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	dpos.Info("Pbft VerifyUncles")
	// TODO panic("implement me")
	return nil
}

func (p *Pbft) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	dpos.Info("Pbft VerifySeal")
	return p.verifySeal(chain, header, nil)
}

func (p *Pbft) verifySeal(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}

	// Retrieve the confirm from the header extra-data
	var confirm payload.Confirm
	err := confirm.Deserialize(bytes.NewReader(header.Extra))
	if err != nil {
		return err
	}
	err = p.verifyConfirm(&confirm)
	if err != nil {
		return err
	}
	return nil
}

func (p *Pbft) Prepare(chain consensus.ChainReader, header *types.Header) error {
	log.Info("Pbft Prepare:", "height;", header.Number.Uint64(), "parent", header.ParentHash.String())
	p.isSealOver = false
	if !p.isRecoved {
		return errors.New("wait for recoved states")
	}
	if p.dispatcher.GetConsensusView().IsRunning() && p.enableViewLoop {
		return errors.New("current consensus is running")
	}
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	nowTime := uint64(p.dispatcher.GetNowTime().Unix())
	if header.Number.Cmp(p.chain.Config().PBFTBlock) == 0 {
		p.Start(nowTime)
	} else {
		p.Start(parent.Time)
	}
	if !p.IsOnduty() {
		return errors.New("local singer is not on duty")
	}

	if header.Number.Uint64() <= p.dispatcher.GetFinishedHeight() {
		log.Info("already confirm block")
		return ErrAlreadyConfirmedBlock
	}
	header.Difficulty = parent.Difficulty
	header.Time = parent.Time + p.period
	if header.Time < nowTime {
		header.Time = nowTime
		p.dispatcher.ResetView(nowTime)
	}

	return nil
}

func (p *Pbft) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
	uncles []*types.Header) {
	dpos.Info("Pbft Finalize:", "height:", header.Number.Uint64())
	sealHash := p.SealHash(header)
	hash, _ := ecom.Uint256FromBytes(sealHash.Bytes())
	p.dispatcher.FinishedProposal(header.Number.Uint64(), *hash, header.Time)
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
	p.CleanFinalConfirmedBlock(header.Number.Uint64())
}

func (p *Pbft) FinalizeAndAssemble(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
	uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	dpos.Info("Pbft FinalizeAndAssemble")
	// No block rewards in DPoS, so the state remains as is and uncles are dropped
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)

	// Assemble and return the final block for sealing
	return types.NewBlock(header, txs, nil, receipts), nil
}

func (p *Pbft) Seal(chain consensus.ChainReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	dpos.Info("Pbft Seal:", block.NumberU64())
	if p.account == nil {
		return errors.New("no signer inited")
	}
	if !p.IsOnduty() {
		return errors.New("local singer is not on duty")
	}
	p.BroadBlockMsg(block)

	if err := p.StartProposal(block); err != nil {
		return err
	}
	p.isSealOver = false
	header := block.Header()
	//Waiting for statistics of voting results
	delay := time.Unix(int64(header.Time), 0).Sub(p.dispatcher.GetNowTime())
	log.Info("wait seal time", "delay", delay)
	time.Sleep(delay)
	changeViewTime := p.dispatcher.GetConsensusView().GetChangeViewTime()
	toleranceDelay := changeViewTime.Sub(p.dispatcher.GetNowTime())
	log.Info("changeViewLeftTime", "toleranceDelay", toleranceDelay)
	select {
	case confirm := <-p.confirmCh:
		log.Info("Received confirmCh", "proposal", confirm.Proposal.Hash().String(), "block:", block.NumberU64())
		p.addConfirmToBlock(header, confirm)
		p.isSealOver = true
		break
	case <- p.unConfirmCh:
		log.Warn("proposal is rejected")
		p.isSealOver = true
		return nil
	case <-time.After(toleranceDelay):
		log.Warn("seal time out stop mine")
		p.isSealOver = true
		return nil
	case <-stop:
		log.Warn("pbft seal is stop")
		p.isSealOver = true
		return nil
	}
	finalBlock  := block.WithSeal(header)
	go func() {
		select {
		case results <- finalBlock:
			p.BroadBlockMsg(finalBlock)
			p.CleanFinalConfirmedBlock(block.NumberU64())
		default:
			dpos.Warn("Sealing result is not read by miner", "sealhash", SealHash(header))
		}
	}()

	return nil
}

func (p *Pbft) addConfirmToBlock(header *types.Header, confirm *payload.Confirm) error {
	sealBuf := new(bytes.Buffer)
	if err := confirm.Serialize(sealBuf); err != nil {
		log.Error("confirm serialize error", "error", err)
		return err
	}
	header.Extra = make([]byte, sealBuf.Len())
	copy(header.Extra[:], sealBuf.Bytes()[:])
	sealHash := SealHash(header)
	hash, _ := ecom.Uint256FromBytes(sealHash.Bytes())
	p.dispatcher.FinishedProposal(header.Number.Uint64(), *hash, header.Time)
	return nil
}

func (p *Pbft) onConfirm(confirm *payload.Confirm) error {
	log.Info("--------[onConfirm]------", "proposal:", confirm.Proposal.Hash())
	if p.isSealOver && p.IsOnduty() {
		log.Warn("seal block is over, can't confirm")
		return errors.New("seal block is over, can't confirm")
	}
	err := p.blockPool.AppendConfirm(confirm)
	if err != nil {
		log.Error("Received confirm", "proposal", confirm.Proposal.Hash().String(), "err:", err)
		return err
	}
	if p.IsOnduty() {
		log.Info("on duty, set confirm block")
		p.confirmCh <- confirm
	} else {
		log.Info("not on duty, not broad confirm block")
	}

	return err
}

func (p *Pbft) onUnConfirm(unconfirm *payload.Confirm) error {
	log.Info("--------[onUnConfirm]------", "proposal:", unconfirm.Proposal.Hash())
	if p.isSealOver {
		return errors.New("seal block is over, can't unconfirm")
	}
	if p.IsOnduty() {
		p.unConfirmCh <- unconfirm
	}
	return nil
}

func (p *Pbft) SealHash(header *types.Header) common.Hash {
	return SealHash(header)
}

func (p *Pbft) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {
	dpos.Info("Pbft CalcDifficulty")
	return big.NewInt(1)
}

func (p *Pbft) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "pbft",
		Version:   "1.0",
		Service:   &API{chain: chain, pbft: p},
		Public:    false,
	}}
}

func (p *Pbft) Close() error {
	dpos.Info("Pbft Close")
	p.enableViewLoop = false
	return nil
}

func (p *Pbft) SignersCount() int {
	dpos.Info("Pbft SignersCount")
	count := len(p.dispatcher.GetConsensusView().GetProducers())
	return count
}

// SealHash returns the hash of a block prior to it being sealed.
func SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeSigHeader(hasher, header)
	hasher.Sum(hash[:0])
	return hash
}

func DBlockSealHash(block dpos.DBlock) (hash ecom.Uint256, err error) {
	if b, ok := block.(*types.Block); ok {
		hasher := sha3.NewLegacyKeccak256()
		encodeSigHeader(hasher, b.Header())
		hasher.Sum(hash[:0])
		return hash, nil
	} else {
		return ecom.EmptyHash, errors.New("verifyBlock errror, block is not ethereum block")
	}
}

func PbftRLP(header *types.Header) []byte {
	b := new(bytes.Buffer)
	encodeSigHeader(b, header)
	return b.Bytes()
}

func encodeSigHeader(w io.Writer, header *types.Header) {
	err := rlp.Encode(w, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		//header.Extra[:len(header.Extra)-crypto.SignatureLength], // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
	})
	if err != nil {
		panic("can't encode: " + err.Error())
	}
}

func (p *Pbft) AddDirectLinkPeer(pid peer.PID, addr string) {
	if p.network != nil {
		p.network.AddDirectLinkAddr(pid, addr)
	}
}

func (p *Pbft) GetActivePeersCount() int {
	if p.network != nil {
		return len(p.network.GetActivePeers())
	}
	return 0
}

func (p *Pbft) StartServer() {
	if p.network != nil {
		p.network.Start()
		p.Recover()
	}
}

func (p *Pbft) StopServer() {
	if p.network != nil {
		p.network.Stop()
	}
	p.enableViewLoop = false
}

func (p *Pbft) Start(headerTime uint64) {
	if !p.enableViewLoop {
		p.enableViewLoop = true
		p.dispatcher.GetConsensusView().SetChangViewTime(headerTime)
		go p.changeViewLoop()
	} else {
		p.dispatcher.ResetView(headerTime)
	}
	p.dispatcher.GetConsensusView().SetRunning()
}

func (p *Pbft) changeViewLoop() {
	for p.enableViewLoop {
		p.network.PostChangeViewTask()
		time.Sleep(1 * time.Second)
	}
}

func (p *Pbft) Recover() {
	if p.IsCurrent == nil || p.account == nil || p.isRecovering ||
		!p.dispatcher.IsProducer(p.account.PublicKeyBytes()) {
		return
	}
	p.isRecovering = true
	for {
		if p.IsCurrent() && len(p.network.GetActivePeers()) > 0 &&
			p.dispatcher.GetConsensusView().HasArbitersMinorityCount(len(p.network.GetActivePeers())) {
			log.Info("----- PostRecoverTask --------")
			p.network.PostRecoverTask()
			p.isRecovering = false
			return
		}
		time.Sleep(time.Second)
	}
}

func (p *Pbft) IsOnduty() bool {
	if p.account == nil {
		return false
	}
	return p.dispatcher.ProducerIsOnDuty()
}

func (p *Pbft) IsProducer() bool {
	if p.account == nil {
		return false
	}
	return p.dispatcher.IsProducer(p.account.PublicKeyBytes())
}

func (p *Pbft) SetBlockChain(chain *core.BlockChain) {
	p.chain = chain
}

func (p *Pbft) broadConfirmMsg(confirm *payload.Confirm, height uint64) {
	msg := emsg.NewConfirmMsg(confirm, height)
	p.network.BroadcastMessage(msg)
}

func (p *Pbft) verifyConfirm(confirm *payload.Confirm) error {
	err := dpos.CheckConfirm(confirm, p.dispatcher.GetConsensusView().GetMajorityCount())
	return err
}

func (p *Pbft) verifyBlock(block dpos.DBlock) error {
	if p.chain == nil {
		return errors.New("pbft chain is nil")
	}
	if b, ok := block.(*types.Block); ok {
		err := p.VerifyHeader(p.chain, b.Header(), false)
		if err != nil {
			return err
		}
		err = p.chain.Validator().ValidateBody(b)
		if err != nil {
			log.Error("validateBody error", "height:", b.GetHeight())
			return err
		}
	} else {
		return errors.New("verifyBlock errror, block is not ethereum block")
	}

	return nil
}

func (p *Pbft) IsInBlockPool(hash common.Hash) bool {
	if u256, err := ecom.Uint256FromBytes(hash.Bytes()); err == nil {
		_, suc := p.blockPool.GetBlock(*u256)
		return suc
	}
	return false
}

func (p *Pbft) CleanFinalConfirmedBlock(height uint64) {
	p.blockPool.CleanFinalConfirmedBlock(height)
}

func (p *Pbft) OnViewChanged(isOnDuty bool, force bool) {
	proposal := p.dispatcher.UpdatePrecociousProposals()
	if proposal != nil {
		log.Info("UpdatePrecociousProposals process proposal")
		p.OnProposalReceived(peer.PID{}, proposal)
	}

	if !force {
		p.dispatcher.CleanProposals(true)
		if p.IsCurrent() && isOnDuty {
			log.Info("---------startMine()-------")
			p.dispatcher.GetConsensusView().SetReady()
			p.StartMine()
		}
	}
}

func (p *Pbft) GetTimeSource() dtime.MedianTimeSource {
	return p.timeSource
}

func (p *Pbft) IsBadBlock(height uint64) bool {
	blocks := p.chain.BadBlocks()
	for _, block := range blocks {
		if block.GetHeight() == height {
			return true
		}
	}
	return false
}