package snapshot

import (
	"runtime"
	"sync"
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// The §4.3 immutability contract, tested concurrently: a writer republishes
// FRESH snapshots in a tight loop while readers assert that a snapshot they
// already Loaded never changes under them. -race cannot catch a violation of
// this (reusing a backing array is not a data race the detector models when
// the pointer swap itself is atomic) — the ASSERTION is the guard.
func TestOldLoadNeverMutatesUnderReader(t *testing.T) {
	var pub Publisher
	stop := make(chan struct{})

	// Writer: fresh snapshot per iteration, contents changing every time.
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			pub.Store(&model.Snapshot{
				Venues: map[model.Venue]model.VenueTop{
					model.Kraken: {
						Venue: model.Kraken, Ready: true, HasBid: true, HasAsk: true,
						BestBid: model.Level{Price: model.Price(i), Qty: model.Qty(i)},
						BestAsk: model.Level{Price: model.Price(i + 1), Qty: model.Qty(i)},
					},
				},
				PublishUnixNanos: i,
			})
		}
	}()

	var readerWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for j := 0; j < 50_000; j++ {
				s := pub.Load()
				if s == nil {
					runtime.Gosched()
					continue
				}
				seq := s.PublishUnixNanos
				bid := s.Venues[model.Kraken].BestBid.Price
				runtime.Gosched() // let the writer republish
				if s.PublishUnixNanos != seq || s.Venues[model.Kraken].BestBid.Price != bid {
					t.Errorf("old snapshot mutated under reader: seq %d->%d bid %d->%d",
						seq, s.PublishUnixNanos, bid, s.Venues[model.Kraken].BestBid.Price)
					return
				}
			}
		}()
	}

	readerWG.Wait()
	close(stop)
	writerWG.Wait()
}

func TestLoadBeforeFirstStoreIsNil(t *testing.T) {
	var pub Publisher
	if s := pub.Load(); s != nil {
		t.Fatalf("Load before first Store = %+v, want nil", s)
	}
}
