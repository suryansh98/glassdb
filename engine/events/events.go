// Package events is glassdb's nervous system: every layer of the engine
// (pager, WAL, B+tree, executor) emits structured events through a Bus so
// that the CLI, the trace recorder, and the X-ray visualizer can watch the
// database work.
//
// A nil *Bus is fully usable: Emit on it is a cheap no-op, so engine code
// never has to guard emit sites.
package events

import "sync"

// Event is a single observation from inside the engine.
type Event struct {
	Seq    uint64         `json:"seq"`
	Type   string         `json:"type"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Event types. The dotted prefix groups events by subsystem; the X-ray UI
// filters on these prefixes.
const (
	EvStmtStart = "stmt.start"
	EvStmtEnd   = "stmt.end"
	EvPlan      = "plan"

	EvPageRead  = "page.read"
	EvPageWrite = "page.write"

	EvCacheHit   = "cache.hit"
	EvCacheMiss  = "cache.miss"
	EvCacheEvict = "cache.evict"
	EvCacheSpill = "cache.spill"

	EvBtreeSearch = "btree.search"
	EvBtreeInsert = "btree.insert"
	EvBtreeSplit  = "btree.split"
	EvBtreeDelete = "btree.delete"

	EvWalAppend       = "wal.append"
	EvWalCommit       = "wal.commit"
	EvWalCheckpoint   = "wal.checkpoint"
	EvWalRecoverBegin = "wal.recover.begin"
	EvWalRecoverFrame = "wal.recover.frame"
	EvWalRecoverEnd   = "wal.recover.end"

	EvTxnBegin    = "txn.begin"
	EvTxnCommit   = "txn.commit"
	EvTxnRollback = "txn.rollback"
)

// Bus fans out engine events to subscribers.
type Bus struct {
	mu   sync.Mutex
	seq  uint64
	subs map[int]chan Event
	next int
	gate func()
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// SetGate installs fn to run before every event is emitted. The serve mode
// uses this to pause, single-step, or slow down the engine so a human can
// watch it think. Pass nil to remove the gate.
func (b *Bus) SetGate(fn func()) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.gate = fn
	b.mu.Unlock()
}

// Emit publishes an event to all subscribers. It never blocks the engine:
// when a subscriber's buffer is full, the oldest buffered event is dropped
// to make room (the X-ray prefers fresh events over complete history).
func (b *Bus) Emit(typ string, fields map[string]any) {
	if b == nil {
		return
	}
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate != nil {
		gate()
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := Event{Seq: b.seq, Type: typ, Fields: fields}
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// Subscribe registers a listener with the given buffer size (default 256)
// and returns the event channel plus a cancel function. Cancel closes the
// channel.
func (b *Bus) Subscribe(buf int) (<-chan Event, func()) {
	if b == nil {
		return nil, func() {}
	}
	if buf <= 0 {
		buf = 256
	}
	ch := make(chan Event, buf)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(ch)
		}
	}
	return ch, cancel
}

// F is shorthand for event fields.
type F = map[string]any
