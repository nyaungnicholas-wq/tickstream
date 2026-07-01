package engine

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/checksum"
	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
	"github.com/nyaungnicholas-wq/tickstream/internal/snapshot"
)

func lv(t testing.TB, price, qty string) model.Level {
	t.Helper()
	p, err := model.PriceFromString(price)
	if err != nil {
		t.Fatal(err)
	}
	q, err := model.QtyFromString(qty)
	if err != nil {
		t.Fatal(err)
	}
	return model.Level{Price: p, Qty: q, PriceStr: price, QtyStr: qty}
}

// tenLevels builds n levels per side around a mid, returning (bids, asks)
// with bids descending from 100.0 and asks ascending from 101.0.
func tenLevels(t testing.TB, n int) (bids, asks []model.Level) {
	t.Helper()
	for i := 0; i < n; i++ {
		bids = append(bids, lv(t, model.Price(int64(1000000000000-int64(i)*100000000)).String(), "1.5"))
		asks = append(asks, lv(t, model.Price(int64(1010000000000+int64(i)*100000000)).String(), "2.5"))
	}
	return bids, asks
}

// krakenChecksumFor computes the checksum the engine should derive for the
// given book contents (top-10 per side).
func krakenChecksumFor(bids, asks []model.Level) uint32 {
	toPairs := func(levels []model.Level) [][2]string {
		out := make([][2]string, 0, len(levels))
		for _, l := range levels {
			if len(out) == 10 {
				break
			}
			out = append(out, [2]string{l.PriceStr, l.QtyStr})
		}
		return out
	}
	return checksum.BookChecksum(toPairs(asks), toPairs(bids))
}

func TestUpdateBeforeSnapshotIsSkipped(t *testing.T) {
	pub := &snapshot.Publisher{}
	e := New(pub, []model.Venue{model.Kraken}, nil, 10)
	e.Apply(model.Event{
		Venue: model.Kraken, Kind: model.KindUpdate,
		Bids: []model.Level{lv(t, "100.0", "1.0")},
	})
	s := pub.Load()
	if s == nil {
		t.Fatal("engine must still publish (a not-ready venue top)")
	}
	if s.Venues[model.Kraken].Ready || s.Venues[model.Kraken].HasBid {
		t.Fatalf("update before snapshot must not populate the book: %+v", s.Venues[model.Kraken])
	}
}

func TestSnapshotThenUpdatePublishesTops(t *testing.T) {
	pub := &snapshot.Publisher{}
	e := New(pub, []model.Venue{model.Kraken}, nil, 10)
	bids, asks := tenLevels(t, 10)
	e.Apply(model.Event{
		Venue: model.Kraken, Kind: model.KindSnapshot,
		Bids: bids, Asks: asks,
		Checksum: krakenChecksumFor(bids, asks),
	})
	s := pub.Load()
	vt := s.Venues[model.Kraken]
	if !vt.Ready || !vt.HasBid || !vt.HasAsk {
		t.Fatalf("venue top not ready after snapshot: %+v", vt)
	}
	if vt.BestBid.PriceStr != "10000" || vt.BestAsk.PriceStr != "10100" {
		t.Fatalf("best bid/ask = %s/%s, want 10000/10100", vt.BestBid.PriceStr, vt.BestAsk.PriceStr)
	}
}

func TestCrossedBookTriggersResync(t *testing.T) {
	pub := &snapshot.Publisher{}
	resynced := 0
	e := New(pub, []model.Venue{model.Coinbase},
		map[model.Venue]func(){model.Coinbase: func() { resynced++ }}, 10)

	e.Apply(model.Event{
		Venue: model.Coinbase, Kind: model.KindSnapshot,
		Bids: []model.Level{lv(t, "100.0", "1.0")},
		Asks: []model.Level{lv(t, "101.0", "1.0")},
	})
	// A bid above the best ask corrupts the single-venue book.
	e.Apply(model.Event{
		Venue: model.Coinbase, Kind: model.KindUpdate,
		Bids: []model.Level{lv(t, "102.0", "1.0")},
	})
	if resynced != 1 {
		t.Fatalf("resync calls = %d, want 1", resynced)
	}
	s := pub.Load()
	if s.Venues[model.Coinbase].Ready {
		t.Fatal("book must be invalidated (not ready) after a crossed update")
	}
}

func TestChecksumMismatchTriggersResync(t *testing.T) {
	pub := &snapshot.Publisher{}
	resynced := 0
	e := New(pub, []model.Venue{model.Kraken},
		map[model.Venue]func(){model.Kraken: func() { resynced++ }}, 10)

	bids, asks := tenLevels(t, 10)
	good := krakenChecksumFor(bids, asks)
	e.Apply(model.Event{
		Venue: model.Kraken, Kind: model.KindSnapshot,
		Bids: bids, Asks: asks, Checksum: good,
	})
	if resynced != 0 {
		t.Fatalf("valid snapshot checksum must not resync (got %d)", resynced)
	}
	// An update whose checksum disagrees with our book state => desync.
	upd := []model.Level{lv(t, "100.5", "9.9")}
	e.Apply(model.Event{
		Venue: model.Kraken, Kind: model.KindUpdate,
		Bids: upd, Checksum: good + 1, // deliberately wrong
	})
	if resynced != 1 {
		t.Fatalf("checksum mismatch must resync (got %d)", resynced)
	}
	if pub.Load().Venues[model.Kraken].Ready {
		t.Fatal("book must be invalidated after checksum mismatch")
	}
}

// Thin-book guard (§6.4): fewer than 10 levels per side must SKIP the
// checksum comparison — no resync storm right after a thin snapshot.
func TestThinBookSkipsChecksumNoResync(t *testing.T) {
	pub := &snapshot.Publisher{}
	resynced := 0
	e := New(pub, []model.Venue{model.Kraken},
		map[model.Venue]func(){model.Kraken: func() { resynced++ }}, 10)

	before := metrics.ChecksumSkippedThin.Load()
	bids, asks := tenLevels(t, 3) // thin: 3 levels per side
	e.Apply(model.Event{
		Venue: model.Kraken, Kind: model.KindSnapshot,
		Bids: bids, Asks: asks,
		Checksum: 12345, // would mismatch if compared
	})
	if resynced != 0 {
		t.Fatalf("thin book must not resync (got %d)", resynced)
	}
	if metrics.ChecksumSkippedThin.Load() != before+1 {
		t.Fatal("thin-book skip must be surfaced as a metric")
	}
	if !pub.Load().Venues[model.Kraken].Ready {
		t.Fatal("thin book stays ready (usable) while checksum is ungated")
	}
}

func TestApplyLatencyStamped(t *testing.T) {
	pub := &snapshot.Publisher{}
	e := New(pub, []model.Venue{model.Kraken}, nil, 10)
	bids, asks := tenLevels(t, 10)
	e.Apply(model.Event{
		Venue: model.Kraken, Kind: model.KindSnapshot,
		Bids: bids, Asks: asks,
		Checksum: krakenChecksumFor(bids, asks),
		Received: time.Now(),
	})
	s := pub.Load()
	if s.ApplyLatencyNanos <= 0 {
		t.Fatalf("ApplyLatencyNanos = %d, want > 0 when Received is stamped", s.ApplyLatencyNanos)
	}
	if s.PublishUnixNanos == 0 {
		t.Fatal("PublishUnixNanos not stamped")
	}
}

// The §4.3 invariant against the REAL engine: while the engine churns through
// updates (republishing constantly), a reader holding an old Load() must
// never observe its contents change. This is what catches an engine that
// "optimizes" by reusing snapshot maps or level slices across publishes.
func TestEngineSnapshotsImmutableUnderChurn(t *testing.T) {
	pub := &snapshot.Publisher{}
	e := New(pub, []model.Venue{model.Kraken}, nil, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan model.Event, 1024)

	var engineWG sync.WaitGroup
	engineWG.Add(1)
	go func() {
		defer engineWG.Done()
		e.Run(ctx, in)
	}()

	// Feeder: seed a snapshot, then hammer best-bid changes.
	bids, asks := tenLevels(t, 10) // built on the test goroutine (t.Fatal-safe)
	var feedWG sync.WaitGroup
	feedWG.Add(1)
	go func() {
		defer feedWG.Done()
		in <- model.Event{
			Venue: model.Kraken, Kind: model.KindSnapshot, Bids: bids, Asks: asks,
			Checksum: krakenChecksumFor(bids, asks),
		}
		for i := int64(0); ; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			qty := model.Qty(100_000_000 + i%97)
			l := model.Level{Price: bids[0].Price, Qty: qty, PriceStr: bids[0].PriceStr, QtyStr: qty.String()}
			// Checksum changes with qty; recompute so no resync fires.
			newBids := append([]model.Level{l}, bids[1:]...)
			select {
			case <-ctx.Done():
				return
			case in <- model.Event{
				Venue: model.Kraken, Kind: model.KindUpdate,
				Bids: []model.Level{l}, Checksum: krakenChecksumFor(newBids, asks),
			}:
			}
		}
	}()

	var readerWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for j := 0; j < 30_000; j++ {
				s := pub.Load()
				if s == nil {
					runtime.Gosched()
					continue
				}
				vt := s.Venues[model.Kraken]
				qty := vt.BestBid.Qty
				pubNanos := s.PublishUnixNanos
				runtime.Gosched() // engine keeps publishing meanwhile
				vt2 := s.Venues[model.Kraken]
				if vt2.BestBid.Qty != qty || s.PublishUnixNanos != pubNanos {
					t.Errorf("engine snapshot mutated under reader: qty %d->%d", qty, vt2.BestBid.Qty)
					return
				}
			}
		}()
	}

	readerWG.Wait()
	cancel()
	feedWG.Wait()
	engineWG.Wait()
}
