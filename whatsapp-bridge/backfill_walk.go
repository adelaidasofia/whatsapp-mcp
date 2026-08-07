// backfill_walk.go — walking backwards through a chat's history so the
// MYC-3284 content backfill can reach past the most recent window.
//
// Why this exists. RequestChatHistory anchors on the NEWEST message in a chat
// and asks WhatsApp for the `count` (max 200) messages before it, so calling it
// repeatedly returns the same window forever. Measured on the live bridge
// 2026-08-01: the first sweep recovered 251 rows, then a further sweep of 40
// chats × 200 delivered 8 more chunks and recovered 0 additional rows. Every
// empty row older than ~200 messages was simply unreachable.
//
// The walk fixes that by feeding the OLDEST message of each delivered chunk
// back in as the next anchor, stepping backwards one window at a time.
//
// The hard part is not the stepping, it is the STOPPING. History arrives
// asynchronously over the events.HistorySync stream, so the walk cannot be a
// loop with a condition at the top — each delivery has to decide whether to
// issue the next request. That shape is self-propelling, and a self-propelling
// process that talks to someone else's server must be bounded by construction,
// not by expecting the right reply. Four independent brakes, any one of which
// halts a chat:
//
//  1. maxRounds        — a hard per-chat ceiling on requests.
//  2. no-progress      — the new chunk's oldest message is not older than the
//                        anchor we asked about, so we are not advancing. This
//                        is the brake that catches "WhatsApp keeps replaying
//                        the same window", the exact failure that motivated
//                        the walk.
//  3. nothing-left     — no empty rows remain older than where we have reached,
//                        so continuing would burn requests for no repair.
//  4. global budget    — a cap across ALL chats, so N concurrent walks cannot
//                        multiply into an unbounded request storm.
//
// State is in-memory on purpose. It is a progress cursor, not a fact about the
// world: losing it on restart costs one redundant window, and the backfill's
// UPDATE is idempotent, so a replayed window is a no-op rather than damage.

package main

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	// defaultMaxWalkRounds bounds requests per chat per sweep. At 200 messages
	// a window this reaches ~4,000 messages back, which covers the deepest
	// chat in the live store (6,245 empty rows, most of them protocol
	// carriers) without unbounded paging.
	defaultMaxWalkRounds = 20

	// defaultWalkBudget bounds requests across ALL chats for one sweep, so a
	// wide sweep cannot multiply per-chat rounds into a request storm.
	defaultWalkBudget = 400

	// walkStaleAfter releases per-chat state when WhatsApp never answers, so
	// an unanswered request cannot pin a chat as "walking" forever and block
	// a later sweep from retrying it.
	walkStaleAfter = 15 * time.Minute
)

// walkState is one chat's position in its backwards walk.
type walkState struct {
	rounds     int
	anchorTS   int64 // timestamp of the anchor we last asked about
	lastReqAt  time.Time
	perChat    int
	stopReason string
}

// backfillWalker coordinates the walks. One instance per Bridge.
type backfillWalker struct {
	mu    sync.Mutex
	chats map[string]*walkState

	spent     int // requests issued this sweep, against budget
	budget    int
	maxRounds int

	// Totals for the sweep, reported back so an operator sees where it stopped
	// rather than inferring it from silence.
	stopped map[string]int // stopReason -> count
}

func newBackfillWalker() *backfillWalker {
	return &backfillWalker{
		chats:     map[string]*walkState{},
		stopped:   map[string]int{},
		budget:    defaultWalkBudget,
		maxRounds: defaultMaxWalkRounds,
	}
}

// reset clears state and re-arms the budget for a new sweep.
func (w *backfillWalker) reset(budget, maxRounds, perChat int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.chats = map[string]*walkState{}
	w.stopped = map[string]int{}
	w.spent = 0
	if budget > 0 {
		w.budget = budget
	}
	if maxRounds > 0 {
		w.maxRounds = maxRounds
	}
	_ = perChat
}

// begin registers the first request for a chat. Returns false when the global
// budget is exhausted, so the caller does not issue the request at all.
func (w *backfillWalker) begin(chatJID string, anchorTS int64, perChat int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.spent >= w.budget {
		w.stopped["global_budget"]++
		return false
	}
	w.spent++
	w.chats[chatJID] = &walkState{
		rounds:    1,
		anchorTS:  anchorTS,
		lastReqAt: time.Now(),
		perChat:   perChat,
	}
	return true
}

// walkDecision is what a delivered chunk should cause next.
type walkDecision struct {
	Continue bool
	Reason   string // why it stopped, when Continue is false
	PerChat  int
}

// next decides whether a chat keeps walking after a chunk arrived whose oldest
// message is chunkOldestTS. hasOlderEmpties reports whether any empty row
// remains strictly older than that.
//
// Every brake lives here, in one place, so the stop conditions can be read and
// tested together rather than being scattered through the delivery path.
func (w *backfillWalker) next(chatJID string, chunkOldestTS int64, hasOlderEmpties bool) walkDecision {
	w.mu.Lock()
	defer w.mu.Unlock()

	st, ok := w.chats[chatJID]
	if !ok {
		// Not a chat we are walking (e.g. an unsolicited history chunk, or a
		// chunk arriving after the walk was released). Do nothing.
		return walkDecision{Continue: false, Reason: "not_walking"}
	}

	stop := func(reason string) walkDecision {
		st.stopReason = reason
		w.stopped[reason]++
		delete(w.chats, chatJID)
		return walkDecision{Continue: false, Reason: reason}
	}

	// Brake 2: we asked for messages before anchorTS and got back a window
	// whose oldest message is not older than that. We are not advancing, so
	// stepping again would replay the same window indefinitely.
	if chunkOldestTS <= 0 || chunkOldestTS >= st.anchorTS {
		return stop("no_progress")
	}
	// Brake 3: nothing left worth fetching further back.
	if !hasOlderEmpties {
		return stop("nothing_older")
	}
	// Brake 1: per-chat ceiling.
	if st.rounds >= w.maxRounds {
		return stop("max_rounds")
	}
	// Brake 4: global budget across all chats.
	if w.spent >= w.budget {
		return stop("global_budget")
	}

	w.spent++
	st.rounds++
	st.anchorTS = chunkOldestTS
	st.lastReqAt = time.Now()
	return walkDecision{Continue: true, PerChat: st.perChat}
}

// sweepStale releases chats whose last request was never answered, so they do
// not stay pinned as "walking" and block a later sweep.
func (w *backfillWalker) sweepStale() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for jid, st := range w.chats {
		if time.Since(st.lastReqAt) > walkStaleAfter {
			w.stopped["unanswered"]++
			delete(w.chats, jid)
		}
	}
}

// stats snapshots progress for the API response.
func (w *backfillWalker) stats() (active, spent, budget int, stopped map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.stopped))
	for k, v := range w.stopped {
		out[k] = v
	}
	return len(w.chats), w.spent, w.budget, out
}

// --- bridge integration ----------------------------------------------------

// hasEmptyRowsOlderThan reports whether the chat still holds pre-MYC-3284
// empty rows strictly older than ts. This is the "is another window worth
// fetching" question, and answering it from the store is what keeps the walk
// from paging through history that has nothing left to repair.
func (b *Bridge) hasEmptyRowsOlderThan(ctx context.Context, chatJID string, ts int64) (bool, error) {
	var n int
	err := b.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM messages
			 WHERE chat_jid = ?
			   AND timestamp < ?
			   AND type = 'system'
			   AND (content_text IS NULL OR content_text = '')
		)
	`, chatJID, ts).Scan(&n)
	return n == 1, err
}

// continueWalk is called once per conversation in a delivered history chunk.
// It decides whether to step further back and, if so, issues the next request
// anchored on that chunk's oldest message.
func (b *Bridge) continueWalk(ctx context.Context, chatJID string, oldest chunkAnchor) {
	if b.walker == nil || oldest.ID == "" {
		return
	}

	hasOlder, err := b.hasEmptyRowsOlderThan(ctx, chatJID, oldest.TS)
	if err != nil {
		log.Printf("backfill walk: older-empties check for %s failed: %v", chatJID, err)
		return
	}

	d := b.walker.next(chatJID, oldest.TS, hasOlder)
	if !d.Continue {
		if d.Reason != "not_walking" {
			log.Printf("backfill walk: %s stopped (%s)", chatJID, d.Reason)
		}
		return
	}

	// Send off the event goroutine. continueWalk runs inside whatsmeow's
	// synchronous event dispatch, and RequestHistoryBefore does a network send
	// that can block for seconds — holding the dispatch there would stall
	// delivery of every other event, including the very history chunks this
	// walk is waiting on.
	//
	// Spawning here is bounded, not open-ended: the walker already decremented
	// the global budget before returning Continue, so the number of goroutines
	// this can create over a sweep is capped by that same budget.
	go func() {
		reqCtx, cancel := context.WithTimeout(b.rootCtx, 30*time.Second)
		defer cancel()
		if _, err := b.RequestHistoryBefore(reqCtx, chatJID, oldest.ID, oldest.TS, oldest.FromMe, d.PerChat); err != nil {
			log.Printf("backfill walk: next request for %s failed: %v", chatJID, err)
			return
		}
		log.Printf("backfill walk: %s stepping back before %s (ts=%d)", chatJID, oldest.ID, oldest.TS)
	}()
}

// chunkAnchor identifies the oldest message in a delivered history chunk,
// which becomes the anchor for the next step backwards.
type chunkAnchor struct {
	ID     string
	TS     int64
	FromMe bool
}

// WalkStats exposes walk progress for the API.
func (b *Bridge) WalkStats() (active, spent, budget int, stopped map[string]int) {
	if b.walker == nil {
		return 0, 0, 0, map[string]int{}
	}
	return b.walker.stats()
}
