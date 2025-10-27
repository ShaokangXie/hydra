// Copyright 2022 IBM Corp. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package log

import (
	"math"
	"math/bits"
	"runtime"
	"sync"
	"time"

	"github.com/hyperledger-labs/hydra/account"
	pb "github.com/hyperledger-labs/hydra/protobufs"
	"github.com/hyperledger-labs/hydra/tracing"
	logger "github.com/rs/zerolog/log"
)

const (
	// Capacity of channels used for subscribing to the log Entries.
	// The goroutine committing a new Entry to the log will block if the channel is full.
	// This should be avoided.
	entryChannelCapacity = 100000

	// Same as above, for checkpoints
	checkpointChannelCapacity = 1000

	finalizedChannelCapacity = 100000000
)

var (
	// All entries indexed by sequence number
	entries = sync.Map{}

	// Channels to which new log entries are pushed IN ORDER.
	// Guarded by entryPublishLock
	logSubscribers = make([]chan *Entry, 0)

	// Channels to which new log entries are pushed OUT OF ORDER.
	// Guarded by entryPublishLock
	logSubscribersOutOfOrder = make([]chan *Entry, 0)

	// Subscribers to particular entries.
	// Map of lists of channels, indexed by sequence numbers.
	// The publishEntries method pushes true to each channel stored
	// under the corresponding sequence number and removes the entry.
	// Protected by entryPublishLock.
	entrySubscribers = make(map[int32][]chan bool)

	// Sequence number of the first empty slot in the log.
	// Advanced when publishing log entries.
	// Guarded by entryPublishLock
	firstEmptySN int32 = 0

	// Guards logSubscribers, logSubscribersOutOfOrder, entrySubscribers and firstEmptySN
	entryPublishLock = sync.Mutex{}

	// The most recent stable checkpoint.
	// Guarded by checkpointLock
	checkpoint *pb.StableCheckpoint

	// Channels to which new checkpoints are pushed.
	// Guarded by checkpointLock
	checkpointSubscribers = make([]chan *pb.StableCheckpoint, 0)

	// Guards checkpoint and checkpointSubscribers variables
	checkpointLock = sync.Mutex{}

	// ---- per-tx replica tracking ----
	txMu        sync.Mutex
	txPending   = make(map[txKey]*repSet)  // Map of pending logical transactions
	txFinalized = make(map[txKey]struct{}) // Set of finalized logical transactions

	// ---- finalized dispatch ----
	finalizedSubs     = make([]chan *Entry, 0)
	finalizedSubsLock sync.Mutex
	finalizedQueue    chan *Entry

	// ---- commit pipeline ----
	commitInitOnce   sync.Once
	commitInCh       chan []*pb.ClientRequest        // The raw committed batches from the log
	commitExecCh     chan []*pb.ClientRequest        // Batches ready for execution
	commitWorkerN    = max(2, runtime.NumCPU())      // Number of commit workers
	commitMaxBatch   = 16_384                        // Maximum number of requests per commit batch
	commitCoalesceNS = int64(200 * time.Microsecond) // Maximum time to wait for coalescing more requests

	entriesFreeMu sync.Mutex
)

type txKey struct {
	ClientID int32
	ClientSn int32
}

// repSet tracks the set of replicas that have committed a particular logical transaction.
type repSet struct {
	total  int32             // Expected number of replicas
	mask   uint64            // Bitset of replicas that have committed
	sample *pb.ClientRequest // A sample ClientRequest for this transaction
	minSN  int32             // Minimum log sequence number in which this transaction was included
}

// CommitEntry a decided value to the log.
// This is the final decision that will never be reverted.
// If this is the first empty slot of the log, push the Entry (and potentially other previously committed entries with
// higher sequence numbers) to the subscribers.
func CommitEntry(entry *Entry) {

	// Only store an entry if it is not yet present.
	// Two different (and even concurrent) stores might occur when an entry is committed normally after the state
	// transfer protocol already has been triggered.
	if _, loaded := entries.LoadOrStore(entry.Sn, entry); loaded {
		logger.Warn().Int32("sn", entry.Sn).Msg("Not overwriting log entry.")
		return
	}

	tracing.MainTrace.Event(tracing.COMMIT, int64(entry.Sn), 0)
	logger.Info().
		Int32("sn", entry.Sn).
		Int("nReq", len(entry.Batch.Requests)).
		Int64("latency", (entry.CommitTs-entry.ProposeTs)/1000000).
		Msg("Committed entry.")

	entryPublishLock.Lock()
	publishEntry(entry, logSubscribersOutOfOrder)
	entryPublishLock.Unlock()

	go func() {
		publishEntries()

		// Enqueue commit for all requests in the entry
		if entry.Batch != nil && len(entry.Batch.Requests) > 0 {
			enqueueCommit(entry.Batch.Requests)
		}

		// Mark replicas and maybe finalize batch
		if entry.Batch != nil && len(entry.Batch.Requests) > 0 {
			markReplicasAndMaybeFinalizeBatch(entry.Batch.Requests, entry.Sn)
		}
	}()
}

// Mark replicas that have committed the given client requests.
func markReplicasAndMaybeFinalizeBatch(crs []*pb.ClientRequest, sn int32) {
	txMu.Lock()

	type finItem struct {
		sample *pb.ClientRequest
		minSN  int32
	}
	finished := make([]finItem, 0, 64)

	for _, cr := range crs {
		if cr == nil || cr.RequestId == nil {
			continue
		}
		rid := cr.RequestId
		key := txKey{ClientID: rid.ClientId, ClientSn: rid.ClientSn}

		if _, done := txFinalized[key]; done {
			continue
		}

		rs, ok := txPending[key]
		if !ok {
			rs = &repSet{
				total:  rid.ClientReplication,
				mask:   0,
				sample: cr,
				minSN:  sn,
			}
			txPending[key] = rs
		} else {
			if rid.ClientReplication > rs.total {
				rs.total = rid.ClientReplication
			}
			if sn < rs.minSN {
				rs.minSN = sn
			}
			if rs.sample == nil || rid.ClientReplicationId < rs.sample.RequestId.ClientReplicationId {
				rs.sample = cr
			}
		}

		if rid.ClientReplicationId >= 0 && rid.ClientReplicationId < 64 {
			rs.mask |= (1 << uint(rid.ClientReplicationId))
		} else {
			logger.Warn().
				Int32("client_id", rid.ClientId).
				Int32("client_sn", rid.ClientSn).
				Int32("replication_id", rid.ClientReplicationId).
				Msg("replication_id >= 64, unexpected under bitset scheme.")
		}

		if rs.total > 0 && bits.OnesCount64(rs.mask) >= int(rs.total) {
			sample, minSN := rs.sample, rs.minSN
			delete(txPending, key)
			txFinalized[key] = struct{}{}
			finished = append(finished, finItem{sample: sample, minSN: minSN})
		}
	}

	txMu.Unlock()

	// Deliver finalized transactions
	minSN := int32(math.MaxInt32)
	allReqs := make([]*pb.ClientRequest, 0, len(finished))
	for _, it := range finished {
		allReqs = append(allReqs, it.sample)
		if it.minSN < minSN {
			minSN = it.minSN
		}
	}
	if minSN == math.MaxInt32 {
		minSN = 0
	}
	fin := &Entry{
		Sn: minSN, // sn does not matter for finalized entries
		Batch: &pb.Batch{
			Requests: append([]*pb.ClientRequest(nil), allReqs...),
		},
	}
	select {
	case finalizedQueue <- fin:
	default:
		logger.Warn().Int32("sn", fin.Sn).Msg("finalizedQueue full; blocking")
		finalizedQueue <- fin
	}
	logger.Info().
		Int32("Length", int32(len(allReqs))).
		Msg("Finalized batch of client requests.")
}

// Retrieve Entry with sequence number sn.
func GetEntry(sn int32) *Entry {
	e, ok := entries.Load(sn)
	if ok {
		return e.(*Entry)
	} else {
		return nil
	}
}

// Returns the sequence numbers of all empty log entries up to (and including) until
func Missing(until int32) []int32 {
	missing := make([]int32, 0)

	entryPublishLock.Lock()
	defer entryPublishLock.Unlock()

	for sn := firstEmptySN; sn <= until; sn++ {
		if _, ok := entries.Load(sn); !ok {
			missing = append(missing, sn)
		}
	}

	return missing
}

// Creates and returns a new channel to which all the new log entries will be pushed in order.
func Entries() chan *Entry {

	newChan := make(chan *Entry, entryChannelCapacity)

	// Use a lock for the case multiple modules subscribe concurrently or the list is being iterated over.
	// OPT: Figure out a way to get rid of this lock, as only a few subscriptions happen at the start and the rest
	//      are read-only iterations until the end of time.
	// TODO: use sync.Map here
	entryPublishLock.Lock()
	logSubscribers = append(logSubscribers, newChan)
	entryPublishLock.Unlock()

	return newChan
}

// Creates and returns a new channel to which all the new log entries will be pushed
// as they are committed, out of order (regardless of potential "holes" in the log).
func EntriesOutOfOrder() chan *Entry {

	newChan := make(chan *Entry, entryChannelCapacity)

	// Use a lock for the case multiple modules subscribe concurrently or the list is being iterated over.
	// OPT: Figure out a way to get rid of this lock, as only a few subscriptions happen at the start and the rest
	//      are read-only iterations until the end of time.
	// TODO: use sync.Map here
	entryPublishLock.Lock()
	logSubscribersOutOfOrder = append(logSubscribersOutOfOrder, newChan)
	entryPublishLock.Unlock()

	return newChan
}

// Blocks until entry with sequence number sn and all previous entries are committed.
// TODO: Do we really want to wait until all previous entries are committed too?
//
//	This can unnecessarily delay a segment just because there is a hole somewhere in the past.
//	(Move the notification to CommitEntry instead of PublishEntries?, If yes, watch out for the lock!)
//	Added after changes to the SimpleCheckpointer:
//	  SimpleCheckpointer relies on the absence of holes guaranteed by WaitForEntry.
//	  The Manager relies on the absence of holes for consistent watermark advancement.
func WaitForEntry(sn int32) {

	// Need this lock to protect from concurrent publishers.
	entryPublishLock.Lock()

	// Entry already committed, return immediately. (Also works for the -1 special value of sn.)
	if firstEmptySN > sn {
		entryPublishLock.Unlock()
		return
	}

	// Entry not yet committed, set up a channel waiting for it.

	// If nobody else is waiting for the entry, allocate list of subscribers.
	if entrySubscribers[sn] == nil {
		entrySubscribers[sn] = make([]chan bool, 0, 1) // In general we expect only one goroutine to wait for an entry.
	}

	// append new channel to the list of subscribers.
	ch := make(chan bool)
	entrySubscribers[sn] = append(entrySubscribers[sn], ch)

	// Wait until a value is pushed in the channel by publishEntries()
	// (the cleanup of entrySubscribers[sn] is performed by the goroutine pushing the value)
	entryPublishLock.Unlock() // Need to unlock, otherwise publishEntries() will never proceed.
	<-ch
}

// If c has a higher sequence number than the most recent stable checkpoint committed so far, CommitCheckpoint()
// replaces the most recent stable checkpoint by c and notifies all subscribers, if any.
func CommitCheckpoint(c *pb.StableCheckpoint) {

	// The lock around this function is required, as concurrent invocations could result in storing a checkpoint that
	// is not the most recent one or in writing the checkpoints to the subscribers out of order
	checkpointLock.Lock()
	defer checkpointLock.Unlock()

	// Check that the new checkpoint's sequence number is higher than the one known so far.
	if checkpoint == nil || c.Sn > checkpoint.Sn {
		checkpoint = c
	} else {
		logger.Warn().
			Int32("oldSn", checkpoint.Sn).
			Int32("newSn", c.Sn).
			Msg("Ignoring new checkpoint with lower or equal sequence number.")
	}

	// Notify all subscribers by pushing the newest checkpoint to the corresponding channels.
	for _, cs := range checkpointSubscribers {
		cs <- c
	}
}

// Returns the latest stable checkpoint, or nil when there is no stable checkpoint.
func GetCheckpoint() *pb.StableCheckpoint {
	// The lock is necessary to prevent race conditions with threads that read the checkpoint concurrently with updates.
	checkpointLock.Lock()
	defer checkpointLock.Unlock()
	return checkpoint
}

// Creates and returns a new channel to which all the new stable checkpoints will be pushed.
// It is possible that a chackpoint will be pushed to this channel without all the entries of that checkpoint being
// committed. This might happen if other peers are faster at producing a checkpoint than this peer is at committing
// entries. If the caller needs the entries to be committed, it can always call log.WaitForEntry(checkpoint.Sn)
// using the returned checkpoint.
func Checkpoints() chan *pb.StableCheckpoint {

	// Protect against multiple goroutines concurrently subscribing or somebody subscribing while a checkpoint is being
	// published (and thus checkpointSubscribers is being iterated over). Extremely unlikely to happen in practice,
	// but still...
	checkpointLock.Lock()
	defer checkpointLock.Unlock()

	newChan := make(chan *pb.StableCheckpoint, checkpointChannelCapacity)
	checkpointSubscribers = append(checkpointSubscribers, newChan)

	return newChan
}

// Pushes committed entries to the subscribers, if any.
func publishEntries() {
	// The lock is necessary for potential concurrent subscribers.
	entryPublishLock.Lock()
	defer entryPublishLock.Unlock()

	// As long as firstEmptySN points to a non-empty slot,
	// push the corresponding Entry to the subscribers
	// and increment firstEmptySN.
	for entry, ok := entries.Load(firstEmptySN); ok; entry, ok = entries.Load(firstEmptySN) {

		logger.Info().
			Int32("sn", firstEmptySN).
			Int("nReq", len(entry.(*Entry).Batch.Requests)).
			Msg("Delivered batch.")

		// On each iteration, push new log Entry to all in-order subscriber channels.
		publishEntry(entry.(*Entry), logSubscribers)

		// Notify entry subscribers
		// The Manager relies on an entry to be published (pushed to all log subscribers)
		// before the entry subscribers are notified.
		if entrySubscribers[firstEmptySN] != nil {
			for _, ch := range entrySubscribers[firstEmptySN] {
				ch <- true
			}
			delete(entrySubscribers, firstEmptySN)
		}

		firstEmptySN++
	}
}

// Publishes a log Entry by writing it to all channels in the subscribers slice.
// The slice must not be concurrently modified, and thus the entryPublishLock must be acquired
// prior to entering this function!
func publishEntry(e *Entry, subscribers []chan *Entry) {

	// TODO: Check if removing the select improves performance
	for _, subscriber := range subscribers {
		select {
		// Try to push entry to channel. (Using the select statement only to output a warning before blocking.)
		case subscriber <- e:
		// Block if channel is full. This should not happen.
		default:
			logger.Warn().Int32("sn", e.Sn).Msg("Failed to push log entry to subscriber channel. Blocking.")
			subscriber <- e
			logger.Warn().Int32("sn", e.Sn).Msg("Unblocking.")
		}
	}
}

func FreeOldEntries(snStart int32, snEnd int32) {
	entriesFreeMu.Lock()
	defer entriesFreeMu.Unlock()

	// 1) First collect all keys to be deleted
	keys := make([]int32, 0, 4096)
	entries.Range(func(k, _ any) bool {
		if sn, ok := k.(int32); ok {
			if sn >= snStart && sn <= snEnd {
				keys = append(keys, sn)
			}
		}
		return true
	})

	// 2) Delete them one by one
	for _, sn := range keys {
		entries.Delete(sn)
	}
}

func init() {
	finalizedQueue = make(chan *Entry, finalizedChannelCapacity)
	go finalizedDispatcher()
}

// ---- commit pipeline ----
func initCommitPipeline() {
	commitInCh = make(chan []*pb.ClientRequest, 4096)
	commitExecCh = make(chan []*pb.ClientRequest, 1024)

	// Coalescing worker: collects requests into bigger batches
	go func() {
		for first := range commitInCh {
			buf := make([]*pb.ClientRequest, 0, len(first)*2)
			buf = append(buf, first...)

			deadline := time.Now().UnixNano() + commitCoalesceNS
			for len(buf) < commitMaxBatch {
				select {
				case more := <-commitInCh:
					buf = append(buf, more...)
					if time.Now().UnixNano() >= deadline {
						goto FLUSH
					}
				default:
					if time.Now().UnixNano() < deadline {
						time.Sleep(20 * time.Microsecond)
						continue
					}
					goto FLUSH
				}
			}
		FLUSH:
			commitExecCh <- buf
		}
	}()

	// Commit workers: execute committed requests
	for i := 0; i < commitWorkerN; i++ {
		go func() {
			for batch := range commitExecCh {
				account.CommitEntry(batch)
			}
		}()
	}
}

func enqueueCommit(reqs []*pb.ClientRequest) {
	commitInitOnce.Do(initCommitPipeline)
	commitInCh <- reqs //
}

// ---- finalized dispatch ----

func finalizedDispatcher() {
	for e := range finalizedQueue {
		finalizedSubsLock.Lock()
		for _, sub := range finalizedSubs {
			select {
			case sub <- e:
			default:
				// Subscriber is slow, drop the entry
				logger.Warn().Int32("sn", e.Sn).Msg("finalized subscriber is slow; dropping one entry")
			}
		}
		finalizedSubsLock.Unlock()
	}
}

func FinalizedEntries() chan *Entry {
	ch := make(chan *Entry, finalizedChannelCapacity)
	finalizedSubsLock.Lock()
	finalizedSubs = append(finalizedSubs, ch)
	finalizedSubsLock.Unlock()
	return ch
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
