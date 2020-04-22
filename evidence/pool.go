package evidence

import (
	"fmt"
	"sync"
	"time"

	dbm "github.com/tendermint/tm-db"

	clist "github.com/tendermint/tendermint/libs/clist"
	"github.com/tendermint/tendermint/libs/log"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/store"
	"github.com/tendermint/tendermint/types"
)

const (
	baseKeyCommitted = byte(0x00) // committed evidence
	baseKeyPending   = byte(0x01) // pending evidence
)

// Pool maintains a pool of valid evidence to be broadcasted and committed..
type Pool struct {
	logger log.Logger

	evidenceStore dbm.DB
	evidenceList  *clist.CList // concurrent linked-list of evidence

	// needed to load validators to verify evidence
	stateDB    dbm.DB
	blockStore *store.BlockStore

	// a map of active validators and respective last heights validator is active
	// if it was in validator set after EvidenceParams.MaxAgeNumBlocks or
	// currently is (ie. [MaxAgeNumBlocks, CurrentHeight])
	// In simple words, it means it's still bonded -> therefore slashable.
	valToLastHeight valToLastHeightMap

	// latest state
	mtx   sync.Mutex
	state sm.State
}

// Validator.Address -> Last height it was in validator set
type valToLastHeightMap map[string]int64

func NewPool(stateDB, evidenceDB dbm.DB, blockStore *store.BlockStore) (*Pool, error) {
	var (
		state = sm.LoadState(stateDB)
	)

	valToLastHeight, err := buildValToLastHeightMap(state, stateDB, blockStore)
	if err != nil {
		return nil, err
	}

	pool := &Pool{
		stateDB:         stateDB,
		blockStore:      blockStore,
		state:           state,
		logger:          log.NewNopLogger(),
		evidenceStore:   evidenceDB,
		evidenceList:    evidenceList,
		valToLastHeight: valToLastHeight,
	}

	// if pending evidence already in db, in event of prior failure, then load it to the evidenceList
	evList := pool.listEvidence(baseKeyPending, -1)
	for _, ev := range evList {
		// check evidence hasn't expired
		if pool.IsExpired(ev) {
			key := keyPending(ev)
			if err := pool.evidenceStore.Delete(key); err != nil {
				pool.logger.Error("Unable to remove expired evidence", "err", err)
			}
			continue
		}
		pool.evidenceList.PushBack(ev)
	}

	return pool, nil
}

// PendingEvidence returns up to maxNum uncommitted evidence.
// If maxNum is -1, all evidence is returned. Pending evidence is in order of priority
func (evpool *Pool) PendingEvidence(maxNum int64) []types.Evidence {
	return evpool.listEvidence(baseKeyPending, maxNum)
}

// Update uses the latest block to update the state, the ValToLastHeight map for evidence expiration
// and to mark committed evidence
func (evpool *Pool) Update(block *types.Block, state sm.State) {
	// sanity check
	if state.LastBlockHeight != block.Height {
		panic(
			fmt.Sprintf("Failed EvidencePool.Update sanity check: got state.Height=%d with block.Height=%d",
				state.LastBlockHeight,
				block.Height,
			),
		)
	}

	// update the state
	evpool.mtx.Lock()
	evpool.state = state
	evpool.mtx.Unlock()

	// remove evidence from pending and mark committed
	evpool.MarkEvidenceAsCommitted(block.Height, block.Time, block.Evidence.Evidence)

	evpool.updateValToLastHeight(block.Height, state)
}

// AddEvidence checks the evidence is valid and adds it to the pool. If
// evidence is composite (ConflictingHeadersEvidence), it will be broken up
// into smaller pieces.
func (evpool *Pool) AddEvidence(evidence types.Evidence) error {
	var (
		state  = evpool.State()
		evList = []types.Evidence{evidence}
	)

	valSet, err := sm.LoadValidators(evpool.stateDB, evidence.Height())
	if err != nil {
		return fmt.Errorf("can't load validators at height #%d: %w", evidence.Height(), err)
	}

	// Break composite evidence into smaller pieces.
	if ce, ok := evidence.(types.CompositeEvidence); ok {
		evpool.logger.Info("Breaking up composite evidence", "ev", evidence)

		blockMeta := evpool.blockStore.LoadBlockMeta(evidence.Height())
		if blockMeta == nil {
			return fmt.Errorf("don't have block meta at height #%d", evidence.Height())
		}

		if err := ce.VerifyComposite(&blockMeta.Header, valSet); err != nil {
			return err
		}

		evList = ce.Split(&blockMeta.Header, valSet, evpool.valToLastHeight)
	}

	for _, ev := range evList {
		if evpool.Has(ev) {
			continue
		}

		// For lunatic validator evidence, a header needs to be fetched.
		var header *types.Header
		if _, ok := ev.(*types.LunaticValidatorEvidence); ok {
			blockMeta := evpool.blockStore.LoadBlockMeta(ev.Height())
			if blockMeta == nil {
				return fmt.Errorf("don't have block meta at height #%d", ev.Height())
			}
			header = &blockMeta.Header
		}

		// 1) Verify against state.
		if err := sm.VerifyEvidence(evpool.stateDB, state, ev, header); err != nil {
			return fmt.Errorf("failed to verify %v: %w", ev, err)
		}

		// 2) Save to store.
		if err := evpool.addPendingEvidence(ev); err != nil {
			return ErrDatabase{err}
		}

		// 3) Add evidence to clist.
		evpool.evidenceList.PushBack(ev)

		evpool.logger.Info("Verified new evidence of byzantine behaviour", "evidence", ev)
	}

	return nil
}

// Evaluates whether the evidence has expired
// FIXME: To be replaced by the ValToLastHeight map
func (evpool *Pool) IsExpired(evidence types.Evidence) bool {
	var (
		params       = evpool.State().ConsensusParams.Evidence
		ageDuration  = evpool.State().LastBlockTime.Sub(evidence.Time())
		ageNumBlocks = evpool.State().LastBlockHeight - evidence.Height()
	)
	return ageNumBlocks > params.MaxAgeNumBlocks &&
		ageDuration > params.MaxAgeDuration
}

// Has checks if the evidence is already stored
func (evpool *Pool) Has(evidence types.Evidence) bool {
	return evpool.IsPending(evidence) || evpool.IsCommitted(evidence)
}

// MarkEvidenceAsCommitted marks all the evidence as committed and removes it
// from the queue.
func (evpool *Pool) MarkEvidenceAsCommitted(height int64, lastBlockTime time.Time, evidence []types.Evidence) {
	// make a map of committed evidence to remove from the clist
	blockEvidenceMap := make(map[string]struct{})
	for _, ev := range evidence {
		// check that evidence has not already been committed
		if evpool.IsCommitted(ev) {
			continue
		}
		// As the evidence is stored in the block store we only need to record the height that it was saved at.
		key := keyCommitted(ev)
		evBytes := cdc.MustMarshalBinaryBare(height)
		if err := evpool.evidenceStore.Set(key, evBytes); err != nil {
			evpool.logger.Error("Unable to add committed evidence", "err", err)
			// if we can't move evidence to committed then don't remove the evidence from pending
			continue
		}
		// if pending, remove from that bucket, remember not all evidence has been seen before
		if evpool.IsPending(ev) {
			key := keyPending(ev)
			if err := evpool.evidenceStore.Delete(key); err != nil {
				evpool.logger.Error("Unable to delete pending evidence", "err", err)
			}
			blockEvidenceMap[evMapKey(ev)] = struct{}{}
		}
	}

	// remove committed evidence from the clist
	if len(blockEvidenceMap) != 0 {
		evidenceParams := evpool.State().ConsensusParams.Evidence
		evpool.removeEvidenceFromList(height, lastBlockTime, evidenceParams, blockEvidenceMap)
	}
}

// Checks whether the evidence has already been committed. DB errors are passed to the logger.
func (evpool *Pool) IsCommitted(evidence types.Evidence) bool {
	key := keyCommitted(evidence)
	ok, err := evpool.evidenceStore.Has(key)
	if err != nil {
		evpool.logger.Error("Unable to find committed evidence", "err", err)
	}
	return ok
}

// Checks whether the evidence is already pending. DB errors are passed to the logger.
func (evpool *Pool) IsPending(evidence types.Evidence) bool {
	key := keyPending(evidence)
	ok, err := evpool.evidenceStore.Has(key)
	if err != nil {
		evpool.logger.Error("Unable to find pending evidence", "err", err)
	}
	return ok
}

func (evpool *Pool) EvidenceFront() *clist.CElement {
	return evpool.evidenceList.Front()
}

func (evpool *Pool) EvidenceWaitChan() <-chan struct{} {
	return evpool.evidenceList.WaitChan()
}

// SetLogger sets the Logger.
func (evpool *Pool) SetLogger(l log.Logger) {
	evpool.logger = l
}

// State returns the current state of the evpool.
func (evpool *Pool) State() sm.State {
	evpool.mtx.Lock()
	defer evpool.mtx.Unlock()
	return evpool.state
}

// AddNewEvidence adds the given evidence to the database.
// It returns false if the evidence is already stored.
func (evpool *Pool) addPendingEvidence(evidence types.Evidence) error {
	var err error
	evBytes := cdc.MustMarshalBinaryBare(evidence)
	key := keyPending(evidence)
	if err = evpool.evidenceStore.Set(key, evBytes); err != nil {
		return err
	}
	return nil
}

// listEvidence lists up to maxNum pieces of evidence for the given prefix key.
// It is wrapped by PriorityEvidence and PendingEvidence for convenience.
// If maxNum is -1, there's no cap on the size of returned evidence.
func (evpool *Pool) listEvidence(prefixKey byte, maxNum int64) (evidence []types.Evidence) {
	var count int64
	iter, err := dbm.IteratePrefix(evpool.evidenceStore, []byte{prefixKey})
	if err != nil {
		panic(err)
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		val := iter.Value()

		if count == maxNum {
			return evidence
		}
		count++

		var ei Info
		err := cdc.UnmarshalBinaryBare(val, &ei)
		if err != nil {
			panic(err)
		}
		evidence = append(evidence, ei.Evidence)
	}
	return evidence
}

func (evpool *Pool) removeEvidenceFromList(
	height int64,
	lastBlockTime time.Time,
	params types.EvidenceParams,
	blockEvidenceMap map[string]struct{}) {

	for e := evpool.evidenceList.Front(); e != nil; e = e.Next() {
		var (
			ev           = e.Value.(types.Evidence)
			ageDuration  = lastBlockTime.Sub(ev.Time())
			ageNumBlocks = height - ev.Height()
		)

		// Remove the evidence if it's already in a block or if it's now too old.
		if _, ok := blockEvidenceMap[evMapKey(ev)]; ok ||
			(ageDuration > params.MaxAgeDuration && ageNumBlocks > params.MaxAgeNumBlocks) {
			// remove from clist
			evpool.evidenceList.Remove(e)
			e.DetachPrev()
		}
	}
}

func evMapKey(ev types.Evidence) string {
	return string(ev.Hash())
}

func (evpool *Pool) updateValToLastHeight(blockHeight int64, state sm.State) {
	// Update current validators & add new ones.
	for _, val := range state.Validators.Validators {
		evpool.valToLastHeight[string(val.Address)] = blockHeight
	}

	// Remove validators outside of MaxAgeNumBlocks & MaxAgeDuration.
	removeHeight := blockHeight - evpool.State().ConsensusParams.Evidence.MaxAgeNumBlocks
	if removeHeight >= 1 {
		valSet, err := sm.LoadValidators(evpool.stateDB, removeHeight)
		if err != nil {
			for _, val := range valSet.Validators {
				h, ok := evpool.valToLastHeight[string(val.Address)]
				if ok && h == removeHeight {
					delete(evpool.valToLastHeight, string(val.Address))
				}
			}
		}
	}
}

func (evpool *Pool) IsExpired(evidence types.Evidence) bool {
	var (
		params       = evpool.State().ConsensusParams.Evidence
		ageDuration  = evpool.State().LastBlockTime.Sub(evidence.Time())
		ageNumBlocks = evpool.State().LastBlockHeight - evidence.Height()
	)
	return ageNumBlocks > params.MaxAgeNumBlocks &&
		ageDuration > params.MaxAgeDuration
}

// listEvidence lists up to maxNum pieces of evidence for the given prefix key.
// It is wrapped by PriorityEvidence and PendingEvidence for convenience.
// If maxNum is -1, there's no cap on the size of returned evidence.
func (evpool *Pool) listEvidence(prefixKey byte, maxNum int64) (evidence []types.Evidence) {
	var count int64
	iter, err := dbm.IteratePrefix(evpool.evidenceStore, []byte{prefixKey})
	if err != nil {
		panic(err)
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		val := iter.Value()

		if count == maxNum {
			return evidence
		}
		count++

		var ei Info
		err := cdc.UnmarshalBinaryBare(val, &ei)
		if err != nil {
			panic(err)
		}
		evidence = append(evidence, ei.Evidence)
	}
	return evidence
}

func evMapKey(ev types.Evidence) string {
	return string(ev.Hash())
}

func buildValToLastHeightMap(state sm.State, stateDB dbm.DB) valToLastHeightMap {
	var (
		valToLastHeight = make(map[string]int64)
		params          = state.ConsensusParams.Evidence

		numBlocks  = int64(0)
		minAgeTime = time.Now().Add(-params.MaxAgeDuration)
		height     = state.LastBlockHeight
	)

	if height == 0 {
		return valToLastHeight, nil
	}

	meta := blockStore.LoadBlockMeta(height)
	if meta == nil {
		return nil, fmt.Errorf("block meta for height %d not found", height)
	}
	blockTime := meta.Header.Time

	// From state.LastBlockHeight, build a map of "active" validators until
	// MaxAgeNumBlocks is passed and block time is less than now() -
	// MaxAgeDuration.
	for height >= 1 && (numBlocks <= params.MaxAgeNumBlocks || !blockTime.Before(minAgeTime)) {
		valSet, err := sm.LoadValidators(stateDB, height)
		if err != nil {
			// last stored height -> return
			if _, ok := err.(sm.ErrNoValSetForHeight); ok {
				return valToLastHeight, nil
			}
			return nil, fmt.Errorf("validator set for height %d not found", height)
		}

		for _, val := range valSet.Validators {
			key := string(val.Address)
			if _, ok := valToLastHeight[key]; !ok {
				valToLastHeight[key] = height
			}
		}

		height--

		if height > 0 {
			// NOTE: we assume here blockStore and state.Validators are in sync. I.e if
			// block N is stored, then validators for height N are also stored in
			// state.
			meta := blockStore.LoadBlockMeta(height)
			if meta == nil {
				return nil, fmt.Errorf("block meta for height %d not found", height)
			}
			blockTime = meta.Header.Time
		}

		numBlocks++
	}

	return valToLastHeight, nil
}

// big endian padded hex
func bE(h int64) string {
	return fmt.Sprintf("%0.16X", h)
}

func keyCommitted(evidence types.Evidence) []byte {
	return append([]byte(baseKeyCommitted), keySuffix(evidence)...)
}

func keyPending(evidence types.Evidence) []byte {
	return append([]byte(baseKeyPending), keySuffix(evidence)...)
}

func keySuffix(evidence types.Evidence) []byte {
	return []byte(fmt.Sprintf("%s/%X", bE(evidence.Height()), evidence.Hash()))
}
