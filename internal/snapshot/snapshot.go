// Package snapshot implements the 1-writer→N-reader atomic snapshot fan-out
// (spec §4.1 rule 3).
//
// Why atomic.Pointer and not a channel or RWMutex: channels are mutex-guarded
// queues (fine for hand-off, wrong for broadcasting the latest value);
// RWMutex is writer-preferring and every RLock writes a shared cache line, so
// it does not scale to many readers. A pointer Load is wait-free.
//
// IMMUTABILITY INVARIANT (§4.3 — a logic race that `-race` CANNOT catch):
// a published *Snapshot, and every slice and map it transitively owns, is
// immutable and freshly allocated per publish. The writer must never mutate a
// snapshot after Store and never reuse a backing array/slice/map across
// publishes. The atomic swap protects the POINTER, not the bytes an old
// pointer still references — a reader holding an old Load() must never see it
// change. The concurrent test in this package (and the engine test) enforce
// this with assertions, not with the race detector.
package snapshot

import (
	"sync/atomic"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// Publisher publishes immutable snapshots to any number of readers.
// The zero value is ready to use.
type Publisher struct {
	p atomic.Pointer[model.Snapshot]
}

// Store publishes a FRESH, fully-built snapshot. The caller must not mutate s
// (or anything it owns) after this call.
func (pub *Publisher) Store(s *model.Snapshot) { pub.p.Store(s) }

// Load returns the current snapshot — wait-free: one atomic pointer load.
// It returns nil before the first publish. Readers may hold the result
// indefinitely; it will never change under them.
func (pub *Publisher) Load() *model.Snapshot { return pub.p.Load() }
