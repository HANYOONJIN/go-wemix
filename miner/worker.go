// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	wemixminer "github.com/ethereum/go-ethereum/wemix/miner"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 102400

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// sealingLogAtDepth is the number of confirmations before logging successful sealing.
	sealingLogAtDepth = 7

	// minRecommitInterval is the minimal time interval to recreate the sealing block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the sealing block with
	// any newly arrived transactions.
	maxRecommitInterval = 15 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 7
)

// environment is the worker's current environment and holds all
// information of the sealing block generation.
type environment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)
	tcount    int            // tx count in cycle
	gasPool   *core.GasPool  // available gas used to pack transactions
	coinbase  common.Address

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
	uncles   map[common.Hash]*types.Header

	// wemix parameters
	till                 *time.Time // until when to block generation holds
	blockInterval        int64
	blockGasLimit        *big.Int
	baseFeeMaxChangeRate int64
	gasTargetPercentage  int64
}

// copy creates a deep copy of environment.
func (env *environment) copy() *environment {
	cpy := &environment{
		signer:    env.signer,
		state:     env.state.Copy(),
		ancestors: env.ancestors.Clone(),
		family:    env.family.Clone(),
		tcount:    env.tcount,
		coinbase:  env.coinbase,
		header:    types.CopyHeader(env.header),
		receipts:  copyReceipts(env.receipts),
	}
	if env.gasPool != nil {
		gasPool := *env.gasPool
		cpy.gasPool = &gasPool
	}
	// The content of txs and uncles are immutable, unnecessary
	// to do the expensive deep copy for them.
	cpy.txs = make([]*types.Transaction, len(env.txs))
	copy(cpy.txs, env.txs)
	cpy.uncles = make(map[common.Hash]*types.Header)
	for hash, uncle := range env.uncles {
		cpy.uncles[hash] = uncle
	}
	return cpy
}

// unclelist returns the contained uncles as the list format.
func (env *environment) unclelist() []*types.Header {
	var uncles []*types.Header
	for _, uncle := range env.uncles {
		uncles = append(uncles, uncle)
	}
	return uncles
}

// discard terminates the background prefetcher go-routine. It should
// always be called for all created environment instances otherwise
// the go-routine leak can happen.
func (env *environment) discard() {
	if env.state == nil {
		return
	}
	env.state.StopPrefetcher()
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts  []*types.Receipt
	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt *int32
	noempty   bool
	timestamp int64
}

// getWorkReq represents a request for getting a new sealing work with provided parameters.
type getWorkReq struct {
	params *generateParams
	err    error
	result chan *types.Block
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config      *Config
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	eth         Backend
	chain       *core.BlockChain

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan core.ChainSideEvent
	chainSideSub event.Subscription

	// Channels
	newWorkCh          chan *newWorkReq
	getWorkCh          chan *getWorkReq
	taskCh             chan *task
	resultCh           chan *types.Block
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	wg sync.WaitGroup

	current      *environment                 // An environment for current running cycle.
	localUncles  map[common.Hash]*types.Block // A set of side blocks generated locally as the possible uncle blocks.
	remoteUncles map[common.Hash]*types.Block // A set of side blocks as the possible uncle blocks.
	unconfirmed  *unconfirmedBlocks           // A set of locally mined blocks pending canonicalness confirmations.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu       sync.RWMutex // The lock used to protect the snapshots below
	snapshotBlock    *types.Block
	snapshotReceipts types.Receipts
	snapshotState    *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// noempty is the flag used to control whether the feature of pre-seal empty
	// block is enabled. The default value is false(pre-seal is enabled by default).
	// But in some special scenario the consensus engine will seal blocks instantaneously,
	// in this case this feature will add all empty blocks into canonical chain
	// non-stop and no real transaction will be included.
	noempty uint32

	// External functions
	isLocalBlock func(header *types.Header) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
}

// compare and swap lock for mining thread
var busyMining int32

func newWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(header *types.Header) bool, init bool) *worker {
	worker := &worker{
		config:             config,
		chainConfig:        chainConfig,
		engine:             engine,
		eth:                eth,
		mux:                mux,
		chain:              eth.BlockChain(),
		isLocalBlock:       isLocalBlock,
		localUncles:        make(map[common.Hash]*types.Block),
		remoteUncles:       make(map[common.Hash]*types.Block),
		unconfirmed:        newUnconfirmedBlocks(eth.BlockChain(), sealingLogAtDepth),
		pendingTasks:       make(map[common.Hash]*task),
		txsCh:              make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:        make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:        make(chan core.ChainSideEvent, chainSideChanSize),
		newWorkCh:          make(chan *newWorkReq, 4),
		getWorkCh:          make(chan *getWorkReq),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		exitCh:             make(chan struct{}),
		startCh:            make(chan struct{}, 1),
		resubmitIntervalCh: make(chan time.Duration),
		resubmitAdjustCh:   make(chan *intervalAdjust, resubmitAdjustChanSize),
	}
	// Subscribe NewTxsEvent for tx pool
	worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	worker.wg.Add(4)
	go worker.mainLoop()
	if wemixminer.IsPoW() {
		go worker.newWorkLoop(recommit)
	} else {
		go worker.newWorkLoopEx(recommit)
	}
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	if init {
		worker.startCh <- struct{}{}
	}
	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

func (w *worker) setGasCeil(ceil uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config.GasCeil = ceil
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	select {
	case w.resubmitIntervalCh <- interval:
	case <-w.exitCh:
	}
}

// disablePreseal disables pre-sealing feature
func (w *worker) disablePreseal() {
	atomic.StoreUint32(&w.noempty, 1)
}

// enablePreseal enables pre-sealing feature
func (w *worker) enablePreseal() {
	atomic.StoreUint32(&w.noempty, 0)
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// pendingBlockAndReceipts returns pending block and corresponding receipts.
func (w *worker) pendingBlockAndReceipts() (*types.Block, types.Receipts) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock, w.snapshotReceipts
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	atomic.StoreInt32(&w.running, 0)
	close(w.exitCh)
	w.wg.Wait()
}

// recalcRecommit recalculates the resubmitting interval upon feedback.
func recalcRecommit(minRecommit, prev time.Duration, target float64, inc bool) time.Duration {
	var (
		prevF = float64(prev.Nanoseconds())
		next  float64
	)
	if inc {
		next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
		max := float64(maxRecommitInterval.Nanoseconds())
		if next > max {
			next = max
		}
	} else {
		next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
		min := float64(minRecommit.Nanoseconds())
		if next < min {
			next = min
		}
	}
	return time.Duration(int64(next))
}

// newWorkLoop is a standalone goroutine to submit new sealing work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	defer w.wg.Done()
	var (
		interrupt   *int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of sealing.
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(noempty bool, s int32) {
		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		select {
		case w.newWorkCh <- &newWorkReq{interrupt: interrupt, noempty: noempty, timestamp: timestamp}:
		case <-w.exitCh:
			return
		}
		timer.Reset(recommit)
		atomic.StoreInt32(&w.newTxs, 0)
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().Unix()
			commit(false, commitInterruptNewHead)

		case head := <-w.chainHeadCh:
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().Unix()
			commit(false, commitInterruptNewHead)

		case <-timer.C:
			// If sealing is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() && (w.chainConfig.Clique == nil || w.chainConfig.Clique.Period > 0) {
				// Short circuit if no new transaction arrives.
				if atomic.LoadInt32(&w.newTxs) == 0 {
					timer.Reset(recommit)
					continue
				}
				commit(true, commitInterruptResubmit)
			}

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				target := float64(recommit.Nanoseconds()) / adjust.ratio
				recommit = recalcRecommit(minRecommit, recommit, target, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recommit = recalcRecommit(minRecommit, recommit, float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// newWorkLoopEx is Wemix's standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoopEx(recommit time.Duration) {
	defer w.wg.Done()

	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()

	// commitSimple just starts a new commitNewWork
	commitSimple := func() {
		if atomic.CompareAndSwapInt32(&busyMining, 0, 1) {
			w.newWorkCh <- &newWorkReq{interrupt: nil, noempty: false, timestamp: time.Now().Unix()}
			atomic.StoreInt32(&w.newTxs, 0)
			atomic.StoreInt32(&busyMining, 0)
		}
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			w.refreshPending(false)
			clearPending(w.chain.CurrentBlock().NumberU64())
			commitSimple()

		case head := <-w.chainHeadCh:
			clearPending(head.Block.NumberU64())
			commitSimple()

		case <-timer.C:
			commitSimple()
			timer.Reset(1 * time.Second)

		case <-w.resubmitIntervalCh:
		case <-w.resubmitAdjustCh:
		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is responsible for generating and submitting sealing work based on
// the received event. It can support two modes: automatically generate task and
// submit it or return task according to given parameters for various proposes.
func (w *worker) mainLoop() {
	defer w.wg.Done()
	defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	defer w.chainSideSub.Unsubscribe()
	defer func() {
		if w.current != nil {
			w.current.discard()
		}
	}()

	cleanTicker := time.NewTicker(time.Second * 10)
	defer cleanTicker.Stop()

	for {
		select {
		case req := <-w.newWorkCh:
			// In wemix, costly interrupt / resubmit is disabled
			if wemixminer.IsPoW() {
				w.commitWork(req.interrupt, req.noempty, req.timestamp)
			} else {
				w.commitWork(nil, req.noempty, req.timestamp)
			}

		case req := <-w.getWorkCh:
			block, err := w.generateWork(req.params)
			if err != nil {
				req.err = err
				req.result <- nil
			} else {
				req.result <- block
			}

		case ev := <-w.chainSideCh:
			// Short circuit for duplicate side blocks
			if _, exist := w.localUncles[ev.Block.Hash()]; exist {
				continue
			}
			if _, exist := w.remoteUncles[ev.Block.Hash()]; exist {
				continue
			}
			// Add side block to possible uncle block set depending on the author.
			if w.isLocalBlock != nil && w.isLocalBlock(ev.Block.Header()) {
				w.localUncles[ev.Block.Hash()] = ev.Block
			} else {
				w.remoteUncles[ev.Block.Hash()] = ev.Block
			}
			// If our sealing block contains less than 2 uncle blocks,
			// add the new uncle block if valid and regenerate a new
			// sealing block for higher profit.
			if w.isRunning() && w.current != nil && len(w.current.uncles) < 2 {
				start := time.Now()
				if err := w.commitUncle(w.current, ev.Block.Header()); err == nil {
					w.commitEx(w.current.copy(), nil, true, start)
				}
			}

		case <-cleanTicker.C:
			chainHead := w.chain.CurrentBlock()
			for hash, uncle := range w.localUncles {
				if uncle.NumberU64()+staleThreshold <= chainHead.NumberU64() {
					delete(w.localUncles, hash)
				}
			}
			for hash, uncle := range w.remoteUncles {
				if uncle.NumberU64()+staleThreshold <= chainHead.NumberU64() {
					delete(w.remoteUncles, hash)
				}
			}

		case ev := <-w.txsCh:
			// Apply transactions to the pending state if we're not sealing
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current sealing block. These transactions will
			// be automatically eliminated.
			if !w.isRunning() && w.current != nil {
				// If block is already full, abort
				if gp := w.current.gasPool; gp != nil && gp.Gas() < params.TxGas {
					continue
				}
				txs := make(map[common.Address]types.Transactions)
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx)
					txs[acc] = append(txs[acc], tx)
				}
				txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs, w.current.header.BaseFee)
				tcount := w.current.tcount
				w.commitTransactions(w.current, txset, nil, nil, nil)

				// Only update the snapshot if any new transactions were added
				// to the pending block
				if tcount != w.current.tcount {
					w.updateSnapshot(w.current)
				}
			} else {
				// Special case, if the consensus engine is 0 period clique(dev mode),
				// submit sealing work here since all empty submission will be rejected
				// by clique. Of course the advance sealing(empty submission) is disabled.
				if w.chainConfig.Clique != nil && w.chainConfig.Clique.Period == 0 {
					w.commitWork(nil, false, time.Now().Unix())
				}
			}
			atomic.AddInt32(&w.newTxs, int32(len(ev.Txs)))

		// System stopped
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		case <-w.chainHeadSub.Err():
			return
		case <-w.chainSideSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	defer w.wg.Done()
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[sealHash] = task
			w.pendingMu.Unlock()

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
				w.pendingMu.Lock()
				delete(w.pendingTasks, sealHash)
				w.pendingMu.Unlock()
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	defer w.wg.Done()
	for {
		select {
		case block := <-w.resultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
			)
			for i, taskReceipt := range task.receipts {
				receipt := new(types.Receipt)
				receipts[i] = receipt
				*receipt = *taskReceipt

				// add block location fields
				receipt.BlockHash = hash
				receipt.BlockNumber = block.Number()
				receipt.TransactionIndex = uint(i)

				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				receipt.Logs = make([]*types.Log, len(taskReceipt.Logs))
				for i, taskLog := range taskReceipt.Logs {
					log := new(types.Log)
					receipt.Logs[i] = log
					*log = *taskLog
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			_, err := w.chain.WriteBlockAndSetHead(block, receipts, logs, task.state, true)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))

			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

		case <-w.exitCh:
			return
		}
	}
}

// makeEnv creates a new environment for the sealing block.
func (w *worker) makeEnv(parent *types.Block, header *types.Header, coinbase common.Address) (*environment, error) {
	// Retrieve the parent state to execute on top and start a prefetcher for
	// the miner to speed block sealing up a bit.
	state, err := w.chain.StateAt(parent.Root())
	if err != nil {
		// Note since the sealing block can be created upon the arbitrary parent
		// block, but the state of parent block may already be pruned, so the necessary
		// state recovery is needed here in the future.
		//
		// The maximum acceptable reorg depth can be limited by the finalised block
		// somehow. TODO(rjl493456442) fix the hard-coded number here later.
		state, err = w.eth.StateAtBlock(parent, 1024, nil, false, false)
		log.Warn("Recovered mining state", "root", parent.Root(), "err", err)
	}
	if err != nil {
		return nil, err
	}
	state.StartPrefetcher("miner")

	// Note the passed coinbase may be different with header.Coinbase.
	env := &environment{
		signer:    types.MakeSigner(w.chainConfig, header.Number),
		state:     state,
		coinbase:  coinbase,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		header:    header,
		uncles:    make(map[common.Hash]*types.Header),
	}
	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range w.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			env.family.Add(uncle.Hash())
		}
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}
	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	return env, nil
}

// commitUncle adds the given block to uncle block set, returns error if failed to add.
func (w *worker) commitUncle(env *environment, uncle *types.Header) error {
	if !wemixminer.IsPoW() {
		return errors.New("We do not call uncles")
	}
	if w.isTTDReached(env.header) {
		return errors.New("ignore uncle for beacon block")
	}
	hash := uncle.Hash()
	if _, exist := env.uncles[hash]; exist {
		return errors.New("uncle not unique")
	}
	if env.header.ParentHash == uncle.ParentHash {
		return errors.New("uncle is sibling")
	}
	if !env.ancestors.Contains(uncle.ParentHash) {
		return errors.New("uncle's parent unknown")
	}
	if env.family.Contains(hash) {
		return errors.New("uncle already included")
	}
	env.uncles[hash] = uncle
	return nil
}

// updateSnapshot updates pending snapshot block, receipts and state.
func (w *worker) updateSnapshot(env *environment) {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.snapshotBlock = types.NewBlock(
		env.header,
		env.txs,
		env.unclelist(),
		env.receipts,
		trie.NewStackTrie(nil),
	)
	w.snapshotReceipts = copyReceipts(env.receipts)
	w.snapshotState = env.state.Copy()
}

func (w *worker) commitTransaction(env *environment, tx *types.Transaction) ([]*types.Log, error) {
	snap := env.state.Snapshot()

	receipt, err := core.ApplyTransaction(w.chainConfig, w.chain, &env.coinbase, env.gasPool, env.state, env.header, tx, &env.header.GasUsed, env.header.Fees, *w.chain.GetVMConfig())
	if err != nil {
		env.state.RevertToSnapshot(snap)
		return nil, err
	}
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)

	return receipt.Logs, nil
}

func (w *worker) commitTransactions(env *environment, txs *types.TransactionsByPriceAndNonce, interrupt *int32, tstart *time.Time, committedTxs map[common.Hash]*types.Transaction) bool {
	gasLimit := env.header.GasLimit
	if env.gasPool == nil {
		env.gasPool = new(core.GasPool).AddGas(gasLimit)
	}
	var coalescedLogs []*types.Log

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the sealing block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(gasLimit-env.gasPool.Gas()) / float64(gasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Break if it has enough transactions
		if params.MaxTxsPerBlock > 0 && env.tcount >= params.MaxTxsPerBlock {
			break
		}
		// Break if it took too long
		if tstart != nil && env.till != nil && time.Until(*env.till) <= 0 && len(committedTxs) >= int(params.BlockMinBuildTxs) {
			break
		}
		// mark it processed
		if committedTxs != nil {
			committedTxs[tx.Hash()] = tx
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), env.tcount)

		logs, err := w.commitTransaction(env, tx)
		switch {
		case errors.Is(err, core.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case errors.Is(err, core.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift()

		case errors.Is(err, core.ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			log.Trace("Skipping unsupported transaction type", "sender", from, "type", tx.Type())
			txs.Pop()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are sealing. The reason is that
		// when we are sealing, the worker will regenerate a sealing block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	return false
}

func (w *worker) commitTransactionsSimple(env *environment, txs *TxOrderer, interrupt *int32, tstart *time.Time) bool {
	gasLimit := env.header.GasLimit
	if env.gasPool == nil {
		env.gasPool = new(core.GasPool).AddGas(gasLimit)
	}
	var coalescedLogs []*types.Log

	tx_prefetch(w, env, txs, params.PrefetchCount)
	defer txs.Close()

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the sealing block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(gasLimit-env.gasPool.Gas()) / float64(gasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Break if it took too long
		if tstart != nil && env.till != nil && time.Until(*env.till) <= 0 && txs.CommittedLength() >= int(params.BlockMinBuildTxs) {
			break
		}
		// mark it processed
		txs.MarkCommitted(tx)

		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), env.tcount)

		logs, err := w.commitTransaction(env, tx)
		switch {
		case errors.Is(err, core.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case errors.Is(err, core.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift()

		case errors.Is(err, core.ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			log.Trace("Skipping unsupported transaction type", "sender", from, "type", tx.Type())
			txs.Pop()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are sealing. The reason is that
		// when we are sealing, the worker will regenerate a sealing block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		w.pendingLogsFeed.Send(cpy)
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	return false
}

// collects ancestors' block times for possible throttling
func (w *worker) ancestorTimes(num *big.Int) []int64 {
	ts := make([]int64, 6)
	for i := 0; i < len(ts); i++ {
		bn := num.Int64()
		switch i {
		case 0:
			bn -= 1
		case 1:
			bn -= 10
		case 2:
			bn -= 50
		case 3:
			bn -= 100
		case 4:
			bn -= 500
		case 5:
			bn -= 1000
		}
		if bn <= 0 {
			continue
		}
		if bh := w.chain.GetHeaderByNumber(uint64(bn)); bh != nil {
			ts[i] = int64(bh.Time)
		}
	}
	return ts
}

// returns throttle delay if necessary in seconds & seconds from the parent
// blocks  seconds  seconds per
//
//	  10        1  0.1
//	  50       10  0.2
//	 100       50  0.5
//	 500      500  1
//	1000     2000  2
func (w *worker) throttleMining(ts []int64) (int64, int64) {
	t := time.Now().Unix()
	dt, pt := int64(0), t-ts[0]

	// 1000th
	if dt = t - ts[5]; ts[5] > 0 && dt < 2000 {
		return 2000 - dt, pt
	}
	if dt = t - ts[4]; ts[4] > 0 && dt < 500 {
		return 500 - dt, pt
	}
	if dt = t - ts[3]; ts[3] > 0 && dt < 50 {
		return 50 - dt, pt
	}
	if dt = t - ts[2]; ts[2] > 0 && dt < 10 {
		return 10 - dt, pt
	}
	return 0, pt
}

func (w *worker) commitTransactionsEx(env *environment, interrupt *int32, tstart time.Time) bool {
	interval := 10 * time.Millisecond

	// committed transactions in this round
	committedTxs := map[common.Hash]*types.Transaction{}
	round := 0
	for {
		round++

		// Fill the block with all available pending transactions.
		pending := w.eth.TxPool().Pending(true)
		// Short circuit if there is no available pending transactions
		if len(pending) == 0 {
			if time.Until(*env.till) <= 0 {
				break
			}
			time.Sleep(interval)
			continue
		}

		// using new simple round-robin ordering instead of old one.
		if params.PrefetchCount == 0 {
			// remove processed txs from 'pending'
			if len(committedTxs) > 0 {
				for k, x := range pending {
					var z types.Transactions
					for _, y := range x {
						if _, ok := committedTxs[y.Hash()]; !ok {
							z = append(z, y)
						}
					}
					if len(z) > 0 {
						pending[k] = z
					} else {
						delete(pending, k)
					}
				}
			}

			txs := types.NewTransactionsByPriceAndNonce(env.signer, pending, env.header.BaseFee)
			if w.commitTransactions(env, txs, interrupt, &tstart, committedTxs) {
				return true
			}
		} else {
			txs := NewTxOrderer(pending, committedTxs)
			if w.commitTransactionsSimple(env, txs, interrupt, &tstart) {
				return true
			}
		}

		if time.Until(*env.till) <= 0 {
			break
		}
		time.Sleep(interval)
		round++
	}
	log.Debug("Block", "number", env.header.Number.Int64(), "elapsed", common.PrettyDuration(time.Since(tstart)), "txs", len(committedTxs), "round", round)

	return false
}

func isBusyMining() bool {
	return atomic.LoadInt32(&busyMining) != 0
}

// generateParams wraps various of settings for generating sealing task.
type generateParams struct {
	timestamp  uint64         // The timstamp for sealing task
	forceTime  bool           // Flag whether the given timestamp is immutable or not
	parentHash common.Hash    // Parent block hash, empty means the latest chain head
	coinbase   common.Address // The fee recipient address for including transaction
	random     common.Hash    // The randomness generated by beacon chain, empty before the merge
	noUncle    bool           // Flag whether the uncle block inclusion is allowed
	noExtra    bool           // Flag whether the extra field assignment is allowed
}

// prepareWork constructs the sealing task according to the given parameters,
// either based on the last chain head or specified parent. In this function
// the pending transactions are not filled yet, only the empty task returned.
func (w *worker) prepareWork(genParams *generateParams) (*environment, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var till time.Time

	// Find the parent block for sealing task
	parent := w.chain.CurrentBlock()
	if genParams.parentHash != (common.Hash{}) {
		parent = w.chain.GetBlockByHash(genParams.parentHash)
	}
	if parent == nil {
		return nil, fmt.Errorf("missing parent")
	}
	blockInterval, _, blockGasLimit, baseFeeMaxChangeRate, gasTargetPercentage, _ := wemixminer.GetBlockBuildParameters(parent.Number())
	// Sanity check the timestamp correctness, recap the timestamp
	// to parent+1 if the mutation is allowed.
	timestamp := genParams.timestamp
	if wemixminer.IsPoW() && parent.Time() >= timestamp {
		if genParams.forceTime {
			return nil, fmt.Errorf("invalid timestamp, parent %d given %d", parent.Time(), timestamp)
		}
		timestamp = parent.Time() + 1
	}
	// Construct the sealing block header, set the extra field if it's allowed
	num := parent.Number()
	num.Add(num, common.Big1)

	// wemix
	if !wemixminer.IsPoW() {
		timestamp, till = w.timeIt(blockInterval)
	}
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num,
		GasLimit:   core.CalcGasLimit(parent.GasLimit(), w.config.GasCeil),
		Fees:       big.NewInt(0),
		Time:       timestamp,
		Coinbase:   genParams.coinbase,
	}
	if !genParams.noExtra && len(w.extra) != 0 {
		header.Extra = w.extra
	}
	if !wemixminer.IsPoW() {
		header.GasLimit = core.CalcGasLimit(parent.GasLimit(), blockGasLimit.Uint64())
	}
	// Set the randomness field from the beacon chain if it's available.
	if genParams.random != (common.Hash{}) {
		header.MixDigest = genParams.random
	}
	// Set baseFee and GasLimit if we are on an EIP-1559 chain
	if w.chainConfig.IsLondon(header.Number) {
		header.BaseFee = misc.CalcBaseFee(w.chainConfig, parent.Header())
		if !w.chainConfig.IsLondon(parent.Number()) {
			if wemixminer.IsPoW() {
				parentGasLimit := parent.GasLimit() * params.ElasticityMultiplier
				header.GasLimit = core.CalcGasLimit(parentGasLimit, w.config.GasCeil)
			} else {
				header.GasLimit = parent.GasLimit()
			}
		}
	}
	// Run the consensus preparation with the default or customized consensus engine.
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for sealing", "err", err)
		return nil, err
	}
	// Could potentially happen if starting to mine in an odd state.
	// Note genParams.coinbase can be different with header.Coinbase
	// since clique algorithm can modify the coinbase field in header.
	env, err := w.makeEnv(parent, header, genParams.coinbase)
	if err != nil {
		log.Error("Failed to create sealing context", "err", err)
		return nil, err
	}
	if !wemixminer.IsPoW() {
		env.till = &till
	}
	env.blockInterval = blockInterval
	env.blockGasLimit = blockGasLimit
	env.baseFeeMaxChangeRate = baseFeeMaxChangeRate
	env.gasTargetPercentage = gasTargetPercentage
	// Accumulate the uncles for the sealing work only if it's allowed.
	if !genParams.noUncle {
		commitUncles := func(blocks map[common.Hash]*types.Block) {
			for hash, uncle := range blocks {
				if len(env.uncles) == 2 {
					break
				}
				if err := w.commitUncle(env, uncle.Header()); err != nil {
					log.Trace("Possible uncle rejected", "hash", hash, "reason", err)
				} else {
					log.Debug("Committing new uncle to block", "hash", hash)
				}
			}
		}
		// Prefer to locally generated uncle
		commitUncles(w.localUncles)
		commitUncles(w.remoteUncles)
	}
	return env, nil
}

// fillTransactions retrieves the pending transactions from the txpool and fills them
// into the given sealing block. The transaction selection and ordering strategy can
// be customized with the plugin in the future.
func (w *worker) fillTransactions(interrupt *int32, env *environment) {
	// Split the pending transactions into locals and remotes
	// Fill the block with all available pending transactions.
	pending := w.eth.TxPool().Pending(true)
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(env.signer, localTxs, env.header.BaseFee)
		if w.commitTransactions(env, txs, interrupt, nil, nil) {
			return
		}
	}
	if len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(env.signer, remoteTxs, env.header.BaseFee)
		if w.commitTransactions(env, txs, interrupt, nil, nil) {
			return
		}
	}
}

// refreshPending reinitialize pending state
func (w *worker) refreshPending(locked bool) {
	if !locked {
		if atomic.CompareAndSwapInt32(&busyMining, 0, 1) {
			defer atomic.StoreInt32(&busyMining, 0)
		} else {
			return
		}
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	parent := w.chain.CurrentBlock()

	blockInterval, _, blockGasLimit, baseFeeMaxChangeRate, gasTargetPercentage, _ := wemixminer.GetBlockBuildParameters(parent.Number())
	num := parent.Number()
	num.Add(num, common.Big1)
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num,
		GasLimit:   core.CalcGasLimit(parent.GasLimit(), w.config.GasCeil),
		Extra:      w.extra,
		Time:       uint64(time.Now().Unix()),
		Fees:       big.NewInt(0),
	}
	if !wemixminer.IsPoW() {
		header.GasLimit = core.CalcGasLimit(parent.GasLimit(), blockGasLimit.Uint64())
	}
	header.Coinbase = w.coinbase
	// Set baseFee and GasLimit if we are on an EIP-1559 chain
	if w.chainConfig.IsLondon(header.Number) {
		header.BaseFee = misc.CalcBaseFee(w.chainConfig, parent.Header())
		if !w.chainConfig.IsLondon(parent.Number()) {
			header.GasLimit = parent.GasLimit()
		}
	}
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	if env, err := w.makeEnv(parent, header, header.Coinbase); err == nil {
		env.blockInterval = blockInterval
		env.blockGasLimit = blockGasLimit
		env.baseFeeMaxChangeRate = baseFeeMaxChangeRate
		env.gasTargetPercentage = gasTargetPercentage
		w.updateSnapshot(env)
	}
}

// generateWork generates a sealing block based on the given parameters.
func (w *worker) generateWork(params *generateParams) (*types.Block, error) {
	work, err := w.prepareWork(params)
	if err != nil {
		return nil, err
	}
	defer work.discard()

	w.fillTransactions(nil, work)
	return w.engine.FinalizeAndAssemble(w.chain, work.header, work.state, work.txs, work.unclelist(), work.receipts)
}

func (w *worker) timeIt(blockInterval int64) (timestamp uint64, till time.Time) {
	if blockInterval /= 1000; blockInterval <= 0 {
		blockInterval = 1
	}

	maxPeekBack := int64(86400)   // don't look back further than this
	tooBehindMultiple := int64(2) // ignore if > tooBehindMultiple * height * blockInterval

	parent := w.chain.CurrentBlock()
	num := parent.Number()
	num.Add(num, common.Big1)
	now := time.Now()
	nowInSeconds := now.Unix()
	nowInMilliSeconds := now.UnixNano() / 1e6 // convert to millisecond

	check := func(heightToPeek int64) (offset int, height, stamp uint64, dt int64) {
		if heightToPeek > maxPeekBack {
			heightToPeek = maxPeekBack
		}
		n := num.Int64() - heightToPeek
		if n < 0 {
			return 0, 0, 0, 0
		}
		h := w.chain.GetHeaderByNumber(uint64(n))
		if h == nil {
			return 0, uint64(n), 0, 0
		}
		offset = 0
		height = uint64(n)
		stamp = h.Time
		dt = nowInSeconds - int64(stamp)
		if heightToPeek*blockInterval < dt && dt < tooBehindMultiple*heightToPeek*blockInterval {
			// behind
			offset = -1
		} else if dt < heightToPeek*blockInterval {
			// ahead
			offset = 1
		}
		return
	}

	ahead := 0
	offset, height, _, dt := check(1)
	log.Debug("time-it", "round", 1, "offset", offset, "height", height, "dt", dt)
	if offset >= 0 {
		if offset > 0 {
			ahead++
		}
		adjBlocks := params.BlockTimeAdjBlocks
		for i := int64(0); i < params.BlockTimeAdjMultiple; i++ {
			offset, height, _, dt = check(adjBlocks)
			log.Debug("time-it", "round", adjBlocks, "offset", offset, "height", height, "dt", dt)
			if offset < 0 {
				break
			} else if offset > 0 {
				ahead++
			}
			adjBlocks *= 10
		}
	}
	if offset >= 0 && ahead > 0 {
		offset = 1
	}
	timestamp = uint64(nowInSeconds)
	if timestamp < parent.Number().Uint64() {
		timestamp = parent.Number().Uint64()
	}
	switch offset {
	case -1: // behind, i.e. too few blocks so far, need to make more
		tms := nowInMilliSeconds + params.BlockMinBuildTime
		if tms/1000 <= int64(parent.Time()) {
			// make sure that no more than 2 blocks have the same timestamp
			tms = (nowInSeconds + 1) * 1000
		}
		till = time.Unix(tms/1e3, (tms%1e3)*1e6)
		log.Debug("time-it", "behind", timestamp, "duration", tms-nowInMilliSeconds)
	case 1: // ahead, i.e. too many blocks, need to slow down
		tms := nowInMilliSeconds + blockInterval*1000 + params.BlockMinBuildTime
		if tms/1000 > nowInSeconds+blockInterval {
			// make sure time stamp doesn't jump by blockInterval + 2
			tms = (nowInSeconds+blockInterval+1)*1000 - params.BlockTrailTime
		}
		till = time.Unix(tms/1e3, (tms%1e3)*1e6)
		log.Debug("time-it", "ahead", timestamp, "duration", tms-nowInMilliSeconds)
	default: // on schedule
		tms := nowInMilliSeconds + blockInterval*1000 - params.BlockTrailTime
		if tms/1000 > nowInSeconds+1 {
			// make sure time stamp doesn't jump by 2
			tms = (nowInSeconds+2)*1000 - params.BlockTrailTime
		}
		till = time.Unix(tms/1e3, (tms%1e3)*1e6)
		log.Debug("time-it", "on-schedule", timestamp, "duration", tms-nowInMilliSeconds)
	}
	return timestamp, till
}

// commitWork generates several new sealing tasks based on the parent block
// and submit them to the sealer.
func (w *worker) commitWork(interrupt *int32, noempty bool, timestamp int64) {
	if atomic.CompareAndSwapInt32(&busyMining, 0, 1) {
		defer atomic.StoreInt32(&busyMining, 0)
	} else {
		return
	}
	if !wemixminer.IsPoW() {
		parent := w.chain.CurrentBlock()
		height := new(big.Int).Add(parent.Number(), common.Big1)
		ok, err := wemixminer.AcquireMiningToken(height, parent.Hash())
		if ok {
			log.Debug("Mining Token, successful", "height", height, "parent-hash", parent.Hash())
		} else {
			log.Debug("Mining Token, failure", "height", height, "parent-hash", parent.Hash(), "error", err)
		}
		if !ok {
			w.refreshPending(true)
			return
		}
	}

	start := time.Now()
	timestamp = start.Unix()

	// Set the coinbase if the worker is running or it's required
	var coinbase common.Address
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
		coinbase = w.coinbase // Use the preset address as the fee recipient
	}
	work, err := w.prepareWork(&generateParams{
		timestamp: uint64(timestamp),
		coinbase:  coinbase,
	})
	if err != nil {
		return
	}

	if !wemixminer.IsPoW() { // Wemix
		if !w.commitTransactionsEx(work, interrupt, start) {
			w.commitEx(work, w.fullTaskHook, true, start)
		}
		return
	}

	// Create an empty block based on temporary copied state for
	// sealing in advance without waiting block execution finished.
	if !noempty && atomic.LoadUint32(&w.noempty) == 0 {
		w.commit(work.copy(), nil, false, start)
	}
	// Fill pending transactions from the txpool
	w.fillTransactions(interrupt, work)
	w.commit(work.copy(), w.fullTaskHook, true, start)

	// Swap out the old work with the new one, terminating any leftover
	// prefetcher processes in the mean time and starting a new one.
	if w.current != nil {
		w.current.discard()
	}
	w.current = work
}

// nolint: govet
// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
// Note the assumption is held that the mutation is allowed to the passed env, do
// the deep copy first.
func (w *worker) commit(env *environment, interval func(), update bool, start time.Time) error {
	if !wemixminer.IsPoW() && !wemixminer.HasMiningToken() {
		return errors.New("Not Miner")
	}

	if w.isRunning() {
		if interval != nil {
			interval()
		}
		// Create a local environment copy, avoid the data race with snapshot state.
		// https://github.com/ethereum/go-ethereum/issues/24299
		env := env.copy()
		block, err := w.engine.FinalizeAndAssemble(w.chain, env.header, env.state, env.txs, env.unclelist(), env.receipts)
		if err != nil {
			return err
		}
		// If we're post merge, just ignore
		if !w.isTTDReached(block.Header()) {
			select {
			case w.taskCh <- &task{receipts: env.receipts, state: env.state, block: block, createdAt: time.Now()}:
				w.unconfirmed.Shift(block.NumberU64() - 1)
				log.Info("Commit new sealing work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()),
					"uncles", len(env.uncles), "txs", env.tcount,
					"gas", block.GasUsed(), "fees", totalFees(block, env.receipts),
					"elapsed", common.PrettyDuration(time.Since(start)))

			case <-w.exitCh:
				log.Info("Worker has exited")
			}
		}
	}
	if update {
		w.updateSnapshot(env)
	}
	return nil
}

// In Wemix, uncles are not welcome and difficulty is so low,
// there's no reason to run miners asynchronously.
func (w *worker) commitEx(env *environment, interval func(), update bool, start time.Time) error {
	if !wemixminer.IsPoW() && !wemixminer.HasMiningToken() {
		return errors.New("Not Miner")
	}
	if w.isRunning() {
		// Create a local environment copy, avoid the data race with snapshot state.
		// https://github.com/ethereum/go-ethereum/issues/24299
		env := env.copy()
		block, err := w.engine.FinalizeAndAssemble(w.chain, env.header, env.state, env.txs, env.unclelist(), env.receipts)
		if err != nil {
			return err
		}
		// If we're post merge, just ignore
		if !w.isTTDReached(block.Header()) {
			createdAt := time.Now()
			w.unconfirmed.Shift(block.NumberU64() - 1)

			log.Info("Commit new sealing work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()),
				"uncles", len(env.uncles), "txs", env.tcount,
				"gas", block.GasUsed(), "fees", totalFees(block, env.receipts),
				"elapsed", common.PrettyDuration(time.Since(start)))

			var sealedBlock *types.Block
			stopCh := make(chan struct{})
			resultCh := make(chan *types.Block, 1)
			if err := w.engine.Seal(w.chain, block, resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
			} else {
				sealedBlock = <-resultCh
			}
			close(stopCh)
			close(resultCh)

			if sealedBlock != nil && !w.chain.HasBlock(sealedBlock.Hash(), sealedBlock.NumberU64()) {
				var (
					sealhash = w.engine.SealHash(sealedBlock.Header())
					hash     = sealedBlock.Hash()
					receipts = make([]*types.Receipt, len(env.receipts))
					logs     []*types.Log
				)
				for i, taskReceipt := range env.receipts {
					receipt := new(types.Receipt)
					receipts[i] = receipt
					*receipt = *taskReceipt

					// add block location fields
					receipt.BlockHash = hash
					receipt.BlockNumber = block.Number()
					receipt.TransactionIndex = uint(i)

					// Update the block hash in all logs since it is now available and not when the
					// receipt/log of individual transactions were created.
					receipt.Logs = make([]*types.Log, len(taskReceipt.Logs))
					for i, taskLog := range taskReceipt.Logs {
						log := new(types.Log)
						receipt.Logs[i] = log
						*log = *taskLog
						log.BlockHash = hash
					}
					logs = append(logs, receipt.Logs...)
				}
				if !wemixminer.IsPoW() {
					if err = wemixminer.ReleaseMiningToken(sealedBlock.Number(), sealedBlock.Hash(), sealedBlock.ParentHash()); err != nil {
						return err
					}
				}
				// Commit block and state to database.
				_, err := w.chain.WriteBlockAndSetHead(sealedBlock, receipts, logs, env.state, true)
				if err != nil {
					log.Error("Failed writing block to chain", "err", err)
					return err
				}
				log.Info("Successfully sealed new block", "number", sealedBlock.Number(), "sealhash", sealhash, "hash", hash,
					"elapsed", common.PrettyDuration(time.Since(createdAt)))

				// Broadcast the block and announce chain insertion event
				w.mux.Post(core.NewMinedBlockEvent{Block: sealedBlock})

				// Insert the block into the set of pending ones to resultLoop for confirmations
				w.unconfirmed.Insert(sealedBlock.NumberU64(), sealedBlock.Hash())
			}
		}
	}
	if update {
		w.updateSnapshot(env)
	}
	return nil
}

// getSealingBlock generates the sealing block based on the given parameters.
func (w *worker) getSealingBlock(parent common.Hash, timestamp uint64, coinbase common.Address, random common.Hash) (*types.Block, error) {
	req := &getWorkReq{
		params: &generateParams{
			timestamp:  timestamp,
			forceTime:  true,
			parentHash: parent,
			coinbase:   coinbase,
			random:     random,
			noUncle:    true,
			noExtra:    true,
		},
		result: make(chan *types.Block, 1),
	}
	select {
	case w.getWorkCh <- req:
		block := <-req.result
		if block == nil {
			return nil, req.err
		}
		return block, nil
	case <-w.exitCh:
		return nil, errors.New("miner closed")
	}
}

// isTTDReached returns the indicator if the given block has reached the total
// terminal difficulty for The Merge transition.
func (w *worker) isTTDReached(header *types.Header) bool {
	td, ttd := w.chain.GetTd(header.ParentHash, header.Number.Uint64()-1), w.chain.Config().TerminalTotalDifficulty
	return td != nil && ttd != nil && td.Cmp(ttd) >= 0
}

// copyReceipts makes a deep copy of the given receipts.
func copyReceipts(receipts []*types.Receipt) []*types.Receipt {
	result := make([]*types.Receipt, len(receipts))
	for i, l := range receipts {
		cpy := *l
		result[i] = &cpy
	}
	return result
}

// postSideBlock fires a side chain event, only use it for testing.
func (w *worker) postSideBlock(event core.ChainSideEvent) {
	select {
	case w.chainSideCh <- event:
	case <-w.exitCh:
	}
}

// totalFees computes total consumed miner fees in ETH. Block transactions and receipts have to have the same order.
func totalFees(block *types.Block, receipts []*types.Receipt) *big.Float {
	feesWei := new(big.Int)
	for i, tx := range block.Transactions() {
		minerFee, _ := tx.EffectiveGasTip(block.BaseFee())
		feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), minerFee))
	}
	return new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))
}
