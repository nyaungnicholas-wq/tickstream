// Package engine implements THE single writer (spec §4.1 rule 2): exactly one
// goroutine owns every venue book and the consolidator. It is the only code
// that mutates book state, which removes write-write races by construction.
// It scales by sharding symbols across independent single-writers — never by
// adding locks.
package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/book"
	"github.com/nyaungnicholas-wq/tickstream/internal/checksum"
	"github.com/nyaungnicholas-wq/tickstream/internal/consolidate"
	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
	"github.com/nyaungnicholas-wq/tickstream/internal/signals"
	"github.com/nyaungnicholas-wq/tickstream/internal/snapshot"
)

// checksumDepth is the number of levels per side the Kraken checksum covers —
// always the top 10, regardless of subscribed depth (spec §6.5).
const checksumDepth = 10

// Engine consumes normalized Events and publishes immutable Snapshots.
type Engine struct {
	pub    *snapshot.Publisher
	books  map[model.Venue]*book.Book
	resync map[model.Venue]func()
	depth  int // Kraken truncation depth (subscribed depth)
}

// New builds an engine for the given venues. resync maps each venue to its
// feed's RequestResync (may be nil in offline/test runs). depth is the Kraken
// subscribed depth (10 in v1).
func New(pub *snapshot.Publisher, venues []model.Venue, resync map[model.Venue]func(), depth int) *Engine {
	books := make(map[model.Venue]*book.Book, len(venues))
	for _, v := range venues {
		books[v] = book.New()
	}
	if resync == nil {
		resync = map[model.Venue]func(){}
	}
	return &Engine{pub: pub, books: books, resync: resync, depth: depth}
}

// Run consumes events until ctx is canceled. It must be the ONLY goroutine
// calling Apply.
func (e *Engine) Run(ctx context.Context, in <-chan model.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-in:
			e.Apply(ev)
		}
	}
}

// Apply applies one event and publishes a fresh snapshot. Exported for the
// offline/replay path and the benchmark harness; single-writer discipline is
// the caller's responsibility.
func (e *Engine) Apply(ev model.Event) {
	bk, ok := e.books[ev.Venue]
	if !ok {
		return // event for a venue this engine does not track
	}

	switch ev.Kind {
	case model.KindSnapshot:
		bk.ApplySnapshot(ev.Bids, ev.Asks)
		if ev.Venue == model.Kraken {
			bk.Truncate(e.depth)
		}
		if bk.IsCrossedOrLocked() {
			// A crossed FRESH snapshot means the venue sent us garbage.
			slog.Warn("crossed/locked snapshot — resync", "venue", ev.Venue)
			e.triggerResync(ev.Venue, bk)
			break
		}
		if ev.Venue == model.Kraken {
			e.verifyChecksum(bk, ev)
		}

	case model.KindUpdate:
		if !bk.Ready() {
			break // wait for the snapshot first (spec §6.2)
		}
		bk.ApplyUpdate(ev.Bids, ev.Asks)
		if ev.Venue == model.Kraken {
			bk.Truncate(e.depth)
		}
		// Single-venue crossed/locked book = OUR book is corrupt (contrast:
		// a cross-VENUE cross in the consolidated view is normal, §7).
		if bk.IsCrossedOrLocked() {
			slog.Warn("crossed/locked book after update — resync", "venue", ev.Venue)
			e.triggerResync(ev.Venue, bk)
			break
		}
		if ev.Venue == model.Kraken {
			e.verifyChecksum(bk, ev)
		}
	}

	e.publish(ev)
}

// verifyChecksum validates the Kraken CRC32 over the top-10 of each side.
//
// THIN-BOOK GUARD (spec §6.4): right after a fresh snapshot — or whenever a
// side is genuinely thin — a side may hold fewer than 10 levels. Comparing a
// <10-level checksum would mismatch, trigger a resync, produce another thin
// snapshot, and storm. Until Kraken's exact <10-level rule is verified
// against their docs, we SKIP the comparison on thin sides and surface a
// metric instead of resyncing.
func (e *Engine) verifyChecksum(bk *book.Book, ev model.Event) {
	if bk.Depth(model.Bid) < checksumDepth || bk.Depth(model.Ask) < checksumDepth {
		metrics.ChecksumSkippedThin.Add(1)
		return
	}
	asks := topStrings(bk, model.Ask, checksumDepth) // price low->high
	bids := topStrings(bk, model.Bid, checksumDepth) // price high->low
	if got := checksum.BookChecksum(asks, bids); got != ev.Checksum {
		metrics.ChecksumMismatches.Add(1)
		slog.Warn("kraken checksum mismatch — resync", "got", got, "want", ev.Checksum)
		e.triggerResync(ev.Venue, bk)
	}
}

func topStrings(bk *book.Book, side model.Side, n int) [][2]string {
	levels := bk.TopN(side, n)
	out := make([][2]string, 0, len(levels))
	for _, lv := range levels {
		out = append(out, [2]string{lv.PriceStr, lv.QtyStr})
	}
	return out
}

// triggerResync marks the venue's book not-ready (the consolidator ignores it
// until a fresh snapshot rebuilds it) and asks the feed to reconnect.
func (e *Engine) triggerResync(v model.Venue, bk *book.Book) {
	bk.Invalidate()
	metrics.Resyncs.Add(1)
	if fn := e.resync[v]; fn != nil {
		fn()
	}
}

// publish builds a FRESH immutable Snapshot — per-venue tops, consolidated
// BBO, and live signals — and stores it.
//
// §4.3 discipline: the Venues map is freshly allocated every publish, and the
// VenueTop/Level contents are copied by value out of the books — the snapshot
// shares no backing storage with engine state or with any previous snapshot.
func (e *Engine) publish(ev model.Event) {
	venues := make(map[model.Venue]model.VenueTop, len(e.books))
	for v, bk := range e.books {
		vt := model.VenueTop{Venue: v, Ready: bk.Ready()}
		if lv, ok := bk.BestBid(); ok {
			vt.HasBid, vt.BestBid = true, lv
		}
		if lv, ok := bk.BestAsk(); ok {
			vt.HasAsk, vt.BestAsk = true, lv
		}
		if vt.Ready {
			// Ladder depth for readers; TopN returns fresh slices (§4.3).
			vt.Bids = bk.TopN(model.Bid, checksumDepth)
			vt.Asks = bk.TopN(model.Ask, checksumDepth)
		}
		venues[v] = vt
	}

	bbo := consolidate.Consolidate(venues)
	s := &model.Snapshot{
		Venues:            venues,
		Consolidated:      bbo,
		CrossVenueCrossed: bbo.CrossVenueCrossed,
		PublishUnixNanos:  time.Now().UnixNano(),
	}
	if bbo.Valid {
		// The fixed-point book converts to float64 only here, at the
		// signal layer (spec §8).
		pb, pa := bbo.BestBidPrice.Float64(), bbo.BestAskPrice.Float64()
		qb, qa := bbo.BestBidSize.Float64(), bbo.BestAskSize.Float64()
		s.ImbalanceSigned, s.ImbalanceFraction = signals.Imbalance(qb, qa)
		s.WeightedMid = signals.WeightedMid(pb, pa, qb, qa)
		s.Mid = signals.Mid(pb, pa)
		s.SpreadTicks = bbo.Spread
		if bbo.CrossVenueCrossed {
			metrics.CrossVenueCrosses.Add(1)
		}
	}
	if !ev.Received.IsZero() {
		// time.Since uses the monotonic reading stamped by the feed just
		// after the websocket read returned — queue wait included (§9.1).
		s.ApplyLatencyNanos = time.Since(ev.Received).Nanoseconds()
	}
	e.pub.Store(s)
}
