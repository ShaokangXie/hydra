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

package account

import (
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"sync"
	"time"

	pb "github.com/hyperledger-labs/hydra/protobufs"
	logger "github.com/rs/zerolog/log"
)

var (
	balMu   sync.RWMutex
	balance = make(map[int32]float64)

	ErrNonConserved      = errors.New("sum(deltas) must be 0 (fee excluded)")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrOverflow          = errors.New("float overflow")
	mu                   sync.Mutex
	tracker              = make(map[txKey]*txSeen)
)

func setBalance(uid int32, amount float64) {
	balMu.Lock()
	balance[uid] = amount
	balMu.Unlock()
}

// getBalance returns the current balance for a given user ID.
func getBalance(uid int32) float64 {
	balMu.RLock()
	v, ok := balance[uid]
	balMu.RUnlock()
	if !ok {
		return 0.0
	}
	return v
}

type txKey = string // derive a stable key from RequestID

type txSeen struct {
	need int32             // expected replication count for this tx (number of instances it should appear in)
	seen int32             // how many instances have already seen it
	req  *pb.ClientRequest // keep the last full request for execution
}

func txDetKey(req *pb.ClientRequest) txKey {
	// Build a stable key from the RequestID triple (same as previous version)
	if rid := req.GetRequestId(); rid != nil {
		return fmt.Sprintf("cid=%d/sn=%d/rep=%d",
			rid.GetClientId(), rid.GetClientSn(), rid.GetClientReplication())
	}
	// Fallback: hash of the payload
	sum := sha256.Sum256(req.GetPayload())
	return "pl:" + hex.EncodeToString(sum[:8])
}

func txOrderHash(k txKey) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(k))
	return h.Sum64()
}

func lessTxKey(a, b txKey) bool {
	ha, hb := txOrderHash(a), txOrderHash(b)
	if ha == hb {
		return a < b
	}
	return ha < hb
}

// UpdateAndCollectReady tracks how many times each tx has been observed
// and returns the set of requests that have reached the required replication count.
func UpdateAndCollectReady(reqs []*pb.ClientRequest) []*pb.ClientRequest {
	mu.Lock()
	defer mu.Unlock()

	ready := make([]*pb.ClientRequest, 0)

	for _, req := range reqs {
		if req == nil || len(req.Deltas) == 0 {
			continue
		}
		key := txDetKey(req)
		ts := tracker[key]
		if ts == nil {
			ts = &txSeen{
				need: req.RequestId.ClientReplication,
				seen: 0,
				req:  req,
			}
			tracker[key] = ts
		}
		ts.seen++

		// When the expected replication is reached, mark as ready
		if ts.seen >= ts.need {
			ready = append(ready, ts.req)
			delete(tracker, key)
		}
	}
	return ready
}

type objLock struct {
	owner txKey
	waitQ *txMinHeap
}
type txMinHeap []txKey

func (h txMinHeap) Len() int           { return len(h) }
func (h txMinHeap) Less(i, j int) bool { return lessTxKey(h[i], h[j]) }
func (h txMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *txMinHeap) Push(x any)        { *h = append(*h, x.(txKey)) }
func (h *txMinHeap) Pop() any          { old := *h; x := old[len(old)-1]; *h = old[:len(old)-1]; return x }

type engine struct {
	mu      sync.Mutex
	locks   map[int32]*objLock           // object -> lock
	owners  map[txKey]map[int32]struct{} // tx -> objects currently held
	waiting map[txKey]map[int32]struct{} // tx -> objects currently waited for

	waitCh map[txKey]chan struct{} // tx -> per-tx wake-up channel (directed notify)
}

var eng = &engine{
	locks:   make(map[int32]*objLock),
	owners:  make(map[txKey]map[int32]struct{}),
	waiting: make(map[txKey]map[int32]struct{}),
	waitCh:  make(map[txKey]chan struct{}),
}

// ensureWaitChLocked ensures the wait channel exists for tk.
// Must be called while holding e.mu.
func (e *engine) ensureWaitChLocked(tk txKey) chan struct{} {
	ch := e.waitCh[tk]
	if ch == nil {
		ch = make(chan struct{}, 1) // Buffer 1
		e.waitCh[tk] = ch
	}
	return ch
}

func (e *engine) ensureWaitCh(tk txKey) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ensureWaitChLocked(tk)
}

func (e *engine) notifyUnsafe(tk txKey) {
	if ch, ok := e.waitCh[tk]; ok {
		select {
		case ch <- struct{}{}: // successfully enqueued a wake-up signal
		default: // a signal is already pending; skip
		}
	}
}

func removeFromWaitQ(h *txMinHeap, tk txKey) bool {
	for i, v := range *h {
		if v == tk {
			heap.Remove(h, i)
			return true
		}
	}
	return false
}

// releaseSubsetLocked rolls back a subset of objects acquired during the
// current tryLockAll call. Must be invoked while holding e.mu.
func (e *engine) releaseSubsetLocked(tk txKey, objs []int32) {
	for _, obj := range objs {
		ol := e.locks[obj]
		if ol == nil || ol.owner != tk {
			continue
		}
		if ol.waitQ.Len() > 0 {
			next := heap.Pop(ol.waitQ).(txKey)
			ol.owner = next
			if _, ok := e.owners[next]; !ok {
				e.owners[next] = make(map[int32]struct{})
			}
			e.owners[next][obj] = struct{}{}
			if w, ok := e.waiting[next]; ok {
				delete(w, obj)
				if len(w) == 0 {
					delete(e.waiting, next)
				}
			}
			e.notifyUnsafe(next)
		} else {
			ol.owner = ""
		}
		// Remove from tk's ownership set
		delete(e.owners[tk], obj)
	}
}

// tryLockAll implements the Wait-Die policy. It returns (allAcquired, abortedSelf).
func (e *engine) tryLockAll(tk txKey, keys []int32) (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.owners[tk]; !ok {
		e.owners[tk] = make(map[int32]struct{}, len(keys))
	}
	if _, ok := e.waiting[tk]; !ok {
		e.waiting[tk] = make(map[int32]struct{})
	}

	acquiredThisCall := make([]int32, 0, len(keys))
	all := true

	for _, obj := range keys {
		// Already held; skip
		if _, ok := e.owners[tk][obj]; ok {
			continue
		}
		ol := e.locks[obj]
		if ol == nil {
			ol = &objLock{waitQ: &txMinHeap{}}
			heap.Init(ol.waitQ)
			e.locks[obj] = ol
		}

		if ol.owner == "" {
			// Free: take it
			ol.owner = tk
			e.owners[tk][obj] = struct{}{}
			acquiredThisCall = append(acquiredThisCall, obj)
			delete(e.waiting[tk], obj)
			continue
		}

		if ol.owner == tk {
			continue
		}

		// Conflict: decide to wait or abort by "age" (Wait-Die)
		owner := ol.owner
		if lessTxKey(tk, owner) {
			// tk is older, wait
			if _, already := e.waiting[tk][obj]; !already {
				heap.Push(ol.waitQ, tk)
				e.waiting[tk][obj] = struct{}{}
				e.ensureWaitChLocked(tk)
			}
			all = false
			continue
		}

		// tk is younger, abort immediately and roll back locks acquired in this call
		e.releaseSubsetLocked(tk, acquiredThisCall)
		// Clean up any registered waits
		if waits := e.waiting[tk]; waits != nil {
			for o := range waits {
				if l := e.locks[o]; l != nil && l.waitQ != nil {
					removeFromWaitQ(l.waitQ, tk)
				}
			}
			delete(e.waiting, tk)
		}
		return false, true // aborted
	}

	return all, false
}

// releaseAll releases all objects held by tk; it must pass the lock to the next
// waiter (if any) and deliver a wake-up, mirroring releaseSubsetLocked.
func (e *engine) releaseAll(tk txKey) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Collect objects to be released in this call
	objs := make([]int32, 0, len(e.owners[tk]))
	for obj := range e.owners[tk] {
		objs = append(objs, obj)
	}
	e.releaseSubsetLocked(tk, objs)

	// Clean ownership
	delete(e.owners, tk)

	if w, ok := e.waiting[tk]; ok && len(w) == 0 {
		delete(e.waiting, tk)
	}

	delete(e.waitCh, tk)
}

func applyRequest(req *pb.ClientRequest) error {
	if req == nil || len(req.Deltas) == 0 {
		return nil
	}

	agg := make(map[int32]float64, len(req.Deltas))
	var sum float64
	for _, d := range req.Deltas {
		if d == nil || d.AmountDelta == 0 {
			continue
		}
		agg[d.UserId] += d.AmountDelta
		sum += d.AmountDelta
	}
	if math.Abs(sum) > 1e-12 {
		return ErrNonConserved
	}

	tk := txDetKey(req)
	keys := make([]int32, 0, len(agg))
	for uid := range agg {
		keys = append(keys, uid)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	// 2PL: acquire all locks; if not all acquired, either get aborted by Wait-Die
	// or wait on the directed wake-up channel with a small timeout backoff.
	for {
		all, aborted := eng.tryLockAll(tk, keys)
		if aborted {
			return errors.New("aborted by wait-die")
		}
		if all {
			break
		}
		// Wait for a directed wake-up (or fallback timeout)
		ch := eng.ensureWaitCh(tk)
		select {
		case <-ch:
		case <-time.After(2 * time.Millisecond):
		}
	}

	// // Execute: (optional) pre-check balances, then apply and write back
	// for uid, delta := range agg {
	// 	if delta < 0 {
	// 		cur := getBalance(uid)
	// 		if cur+delta < -1e-12 {
	// 			eng.releaseAll(tk)
	// 			return fmt.Errorf("%w: uid=%d need=%f have=%f", ErrInsufficientFunds, uid, -delta, cur)
	// 		}
	// 	}
	// }
	for uid, delta := range agg {
		cur := getBalance(uid)
		setBalance(uid, cur+delta)
	}
	eng.releaseAll(tk)
	return nil
}

// Batched submission
func CommitEntry(requests []*pb.ClientRequest) {
	logger.Debug().
		Int("requestsLen", len(requests)).
		Msg("account CommitEntryWithInstance")

	ready := UpdateAndCollectReady(requests)
	if len(ready) == 0 {
		return
	}

	// execute sequentially; overall parallelism is controlled by
	// the upper-layer worker pool in the log package.
	for _, r := range ready {
		if err := applyRequest(r); err != nil {
			logger.Error().Err(err).Msg("applyRequest failed")
		}
	}

	logger.Info().
		Int("executedReady", len(ready)).
		Msg("CommitEntryWithInstance done")
}
