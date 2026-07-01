// Package book implements the per-venue L2 order book: pure data-structure
// logic, no networking.
//
// Core semantics (universal across venues, spec §6):
//   - An update Qty is the NEW ABSOLUTE size at that price, not a delta:
//     overwrite, never add.
//   - Qty == 0 means DELETE the price level.
//   - A delete for a level not in the book is normal: silently ignored.
//
// # Data structure & complexity (the honest note, spec §6.1)
//
// Each side is a map[Price]Level plus a price slice sorted worst→best, i.e.
// the BEST price sits at the END of the slice (bids ascending, asks
// descending). Consequences:
//
//   - BestBid/BestAsk READ: O(1) (last element).
//   - Delete of the CURRENT BEST: O(1) amortized — pop the last element and
//     delete the map entry; the new best is the new last element. (With the
//     more common best-at-front layout this is the O(n) trap; putting the
//     best at the end moves the memmove cost to the cold path instead.)
//   - Qty change of an existing level: O(1) map overwrite (slice untouched).
//   - Insert/delete of an arbitrary level: O(log P) binary search plus an
//     O(P) worst-case memmove. The memmove is cheapest near the best (where
//     real-feed activity concentrates) and most expensive at the deepest
//     level. At Kraken depth-10 it is negligible; on the full Coinbase book
//     (tens of thousands of levels) a worst-case deep insert is measured —
//     not hidden — by BenchmarkApplyDeepInsert.
//
// A Book is NOT safe for concurrent use: exactly one goroutine (the engine,
// spec §4.1 rule 2) owns and mutates it. Readers see state only via the
// immutable published Snapshot.
package book

import (
	"sort"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// side is one side of the book.
type side struct {
	levels map[model.Price]model.Level
	// prices is sorted worst→best: ascending for bids, descending for asks,
	// so the BEST price is always prices[len(prices)-1].
	prices []model.Price
	isAsk  bool
}

// worse reports whether price a is strictly worse (further from the touch)
// than b on this side.
func (s *side) worse(a, b model.Price) bool {
	if s.isAsk {
		return a > b
	}
	return a < b
}

// searchIdx returns the index of p if present, or the index at which p would
// be inserted to keep the slice sorted worst→best.
func (s *side) searchIdx(p model.Price) int {
	return sort.Search(len(s.prices), func(i int) bool { return !s.worse(s.prices[i], p) })
}

// set upserts a level (qty > 0): absolute overwrite if present, sorted insert
// if new.
func (s *side) set(lv model.Level) {
	if _, ok := s.levels[lv.Price]; ok {
		s.levels[lv.Price] = lv // absolute size: overwrite, never add
		return
	}
	s.levels[lv.Price] = lv
	i := s.searchIdx(lv.Price)
	s.prices = append(s.prices, 0)
	copy(s.prices[i+1:], s.prices[i:])
	s.prices[i] = lv.Price
}

// del removes a level. Deleting an absent level is a documented no-op.
func (s *side) del(p model.Price) {
	if _, ok := s.levels[p]; !ok {
		return
	}
	delete(s.levels, p)
	i := s.searchIdx(p)
	// Deleting the best (i == len-1) is a pure pop: no memmove.
	s.prices = append(s.prices[:i], s.prices[i+1:]...)
}

func (s *side) best() (model.Level, bool) {
	if len(s.prices) == 0 {
		return model.Level{}, false
	}
	return s.levels[s.prices[len(s.prices)-1]], true
}

func (s *side) clear() {
	s.levels = make(map[model.Price]model.Level)
	s.prices = s.prices[:0]
}

// truncate keeps only the best n levels (the slice tail).
func (s *side) truncate(n int) {
	if n < 0 || len(s.prices) <= n {
		return
	}
	evict := s.prices[:len(s.prices)-n]
	for _, p := range evict {
		delete(s.levels, p)
	}
	s.prices = append(s.prices[:0], s.prices[len(s.prices)-n:]...)
}

// topN returns the best n levels, best-first, as a FRESH slice with no shared
// backing storage (spec §4.3: callers may retain it indefinitely).
func (s *side) topN(n int) []model.Level {
	if n > len(s.prices) {
		n = len(s.prices)
	}
	out := make([]model.Level, 0, n)
	for i := len(s.prices) - 1; i >= len(s.prices)-n; i-- {
		out = append(out, s.levels[s.prices[i]])
	}
	return out
}

// Book is one venue's L2 order book.
type Book struct {
	bids, asks side
	ready      bool
}

// New returns an empty, not-ready Book.
func New() *Book {
	b := &Book{
		bids: side{levels: make(map[model.Price]model.Level)},
		asks: side{levels: make(map[model.Price]model.Level), isAsk: true},
	}
	return b
}

// ApplySnapshot replaces the whole book with the snapshot contents and marks
// the book ready.
func (b *Book) ApplySnapshot(bids, asks []model.Level) {
	b.bids.clear()
	b.asks.clear()
	b.applyLevels(bids, asks)
	b.ready = true
}

// ApplyUpdate upserts each level; Qty == 0 deletes the level (absolute-size
// semantics — see the package comment).
func (b *Book) ApplyUpdate(bids, asks []model.Level) {
	b.applyLevels(bids, asks)
}

func (b *Book) applyLevels(bids, asks []model.Level) {
	for _, lv := range bids {
		if lv.Qty == 0 {
			b.bids.del(lv.Price)
		} else {
			b.bids.set(lv)
		}
	}
	for _, lv := range asks {
		if lv.Qty == 0 {
			b.asks.del(lv.Price)
		} else {
			b.asks.set(lv)
		}
	}
}

// Truncate keeps only the best `depth` levels per side (Kraken: the feed is
// depth-limited and the checksum is defined over the top 10 after truncation).
func (b *Book) Truncate(depth int) {
	b.bids.truncate(depth)
	b.asks.truncate(depth)
}

// BestBid returns the highest bid. ok is false when the side is empty.
// O(1) read.
func (b *Book) BestBid() (model.Level, bool) { return b.bids.best() }

// BestAsk returns the lowest ask. ok is false when the side is empty.
// O(1) read.
func (b *Book) BestAsk() (model.Level, bool) { return b.asks.best() }

// Ready reports whether the book has been initialized by a snapshot.
func (b *Book) Ready() bool { return b.ready }

// Invalidate clears the book and marks it not-ready (resync path, spec §6.7).
func (b *Book) Invalidate() {
	b.bids.clear()
	b.asks.clear()
	b.ready = false
}

// Depth returns the number of levels currently held on a side.
func (b *Book) Depth(s model.Side) int {
	if s == model.Ask {
		return len(b.asks.prices)
	}
	return len(b.bids.prices)
}

// TopN returns the best n levels of a side, best-first, as a fresh slice.
func (b *Book) TopN(s model.Side, n int) []model.Level {
	if s == model.Ask {
		return b.asks.topN(n)
	}
	return b.bids.topN(n)
}

// IsCrossedOrLocked reports bestBid >= bestAsk (both present). Within a
// SINGLE venue this means the local book is corrupt and must be resynced
// (locked = equal, crossed = bid > ask). Contrast spec §7: a cross-venue
// cross in the consolidated book is normal and transient.
func (b *Book) IsCrossedOrLocked() bool {
	bb, okB := b.bids.best()
	ba, okA := b.asks.best()
	return okB && okA && bb.Price >= ba.Price
}
