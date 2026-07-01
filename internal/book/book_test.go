package book

import (
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// lv builds a Level from decimal strings; test helper.
func lv(t testing.TB, price, qty string) model.Level {
	t.Helper()
	p, err := model.PriceFromString(price)
	if err != nil {
		t.Fatalf("price %q: %v", price, err)
	}
	q, err := model.QtyFromString(qty)
	if err != nil {
		t.Fatalf("qty %q: %v", qty, err)
	}
	return model.Level{Price: p, Qty: q, PriceStr: price, QtyStr: qty}
}

func mustPrice(t testing.TB, s string) model.Price {
	t.Helper()
	p, err := model.PriceFromString(s)
	if err != nil {
		t.Fatalf("price %q: %v", s, err)
	}
	return p
}

// snapshotBook returns a ready book:
//
//	bids: 100.5(1.0)  100.0(2.0)  99.5(3.0)
//	asks: 101.0(1.5)  101.5(2.5)  102.0(3.5)
func snapshotBook(t testing.TB) *Book {
	t.Helper()
	b := New()
	b.ApplySnapshot(
		[]model.Level{lv(t, "100.0", "2.0"), lv(t, "100.5", "1.0"), lv(t, "99.5", "3.0")},
		[]model.Level{lv(t, "101.5", "2.5"), lv(t, "101.0", "1.5"), lv(t, "102.0", "3.5")},
	)
	return b
}

func TestBook(t *testing.T) {
	type check func(t *testing.T, b *Book)

	expectBest := func(side model.Side, price, qty string) check {
		return func(t *testing.T, b *Book) {
			t.Helper()
			var got model.Level
			var ok bool
			if side == model.Bid {
				got, ok = b.BestBid()
			} else {
				got, ok = b.BestAsk()
			}
			if !ok {
				t.Fatalf("best %v: side empty", side)
			}
			if got.Price != mustPrice(t, price) || got.QtyStr != qty {
				t.Fatalf("best %v = %s@%s, want %s@%s", side, got.QtyStr, got.PriceStr, qty, price)
			}
		}
	}

	tests := []struct {
		name   string
		mutate func(t *testing.T, b *Book)
		checks []check
	}{
		{
			name:   "1: snapshot builds sorted book with correct bests",
			mutate: func(t *testing.T, b *Book) {},
			checks: []check{
				expectBest(model.Bid, "100.5", "1.0"),
				expectBest(model.Ask, "101.0", "1.5"),
				func(t *testing.T, b *Book) {
					bids := b.TopN(model.Bid, 3)
					for i, want := range []string{"100.5", "100.0", "99.5"} {
						if bids[i].Price != mustPrice(t, want) {
							t.Fatalf("bids[%d] = %s, want %s (descending)", i, bids[i].PriceStr, want)
						}
					}
					asks := b.TopN(model.Ask, 3)
					for i, want := range []string{"101.0", "101.5", "102.0"} {
						if asks[i].Price != mustPrice(t, want) {
							t.Fatalf("asks[%d] = %s, want %s (ascending)", i, asks[i].PriceStr, want)
						}
					}
				},
			},
		},
		{
			name: "2: update OVERWRITES absolute size, does not add",
			mutate: func(t *testing.T, b *Book) {
				b.ApplyUpdate([]model.Level{lv(t, "100.5", "9.9")}, nil)
			},
			checks: []check{expectBest(model.Bid, "100.5", "9.9")},
		},
		{
			name: "3: qty 0 deletes; deleting an absent level is a no-op",
			mutate: func(t *testing.T, b *Book) {
				b.ApplyUpdate([]model.Level{lv(t, "100.0", "0")}, nil) // delete mid level
				b.ApplyUpdate([]model.Level{lv(t, "50.0", "0")}, nil)  // absent: no-op, no panic
			},
			checks: []check{
				func(t *testing.T, b *Book) {
					if b.Depth(model.Bid) != 2 {
						t.Fatalf("bid depth = %d, want 2", b.Depth(model.Bid))
					}
				},
				expectBest(model.Bid, "100.5", "1.0"),
			},
		},
		{
			name: "4: inserting a new better bid/ask updates the best",
			mutate: func(t *testing.T, b *Book) {
				b.ApplyUpdate(
					[]model.Level{lv(t, "100.7", "0.4")},
					[]model.Level{lv(t, "100.9", "0.6")},
				)
			},
			checks: []check{
				expectBest(model.Bid, "100.7", "0.4"),
				expectBest(model.Ask, "100.9", "0.6"),
			},
		},
		{
			name: "5: DELETE-OF-CURRENT-BEST re-derives the next best (both sides)",
			mutate: func(t *testing.T, b *Book) {
				b.ApplyUpdate(
					[]model.Level{lv(t, "100.5", "0")},
					[]model.Level{lv(t, "101.0", "0")},
				)
			},
			checks: []check{
				expectBest(model.Bid, "100.0", "2.0"),
				expectBest(model.Ask, "101.5", "2.5"),
			},
		},
		{
			name: "6: Truncate keeps exactly the top depth per side",
			mutate: func(t *testing.T, b *Book) {
				b.Truncate(2)
			},
			checks: []check{
				func(t *testing.T, b *Book) {
					if b.Depth(model.Bid) != 2 || b.Depth(model.Ask) != 2 {
						t.Fatalf("depth = %d/%d, want 2/2", b.Depth(model.Bid), b.Depth(model.Ask))
					}
				},
				expectBest(model.Bid, "100.5", "1.0"),
				expectBest(model.Ask, "101.0", "1.5"),
				func(t *testing.T, b *Book) {
					// The evicted (worst) levels are gone; the survivors are the best two.
					bids := b.TopN(model.Bid, 10)
					if len(bids) != 2 || bids[1].Price != mustPrice(t, "100.0") {
						t.Fatalf("truncate kept wrong bid levels: %+v", bids)
					}
				},
			},
		},
		{
			name: "7: crossed/locked detection",
			mutate: func(t *testing.T, b *Book) {
				// Insert a bid ABOVE the best ask: deliberately corrupt.
				b.ApplyUpdate([]model.Level{lv(t, "101.2", "1.0")}, nil)
			},
			checks: []check{
				func(t *testing.T, b *Book) {
					if !b.IsCrossedOrLocked() {
						t.Fatal("IsCrossedOrLocked() = false for a crossed book")
					}
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := snapshotBook(t)
			if b.IsCrossedOrLocked() {
				t.Fatal("fresh snapshot book must not be crossed")
			}
			tt.mutate(t, b)
			for _, c := range tt.checks {
				c(t, b)
			}
		})
	}
}

func TestLockedBookDetected(t *testing.T) {
	b := snapshotBook(t)
	// Bid exactly equal to best ask => locked.
	b.ApplyUpdate([]model.Level{lv(t, "101.0", "1.0")}, nil)
	if !b.IsCrossedOrLocked() {
		t.Fatal("IsCrossedOrLocked() = false for a locked book (bid == ask)")
	}
}

// Case 8: TopN returns a fresh slice — mutating it must not affect the book.
func TestTopNReturnsFreshSlice(t *testing.T) {
	b := snapshotBook(t)
	top := b.TopN(model.Bid, 2)
	top[0] = lv(t, "1.0", "1.0") // caller scribbles on the returned slice
	best, ok := b.BestBid()
	if !ok || best.Price != mustPrice(t, "100.5") {
		t.Fatalf("book mutated through TopN slice: best = %+v", best)
	}
	// Two successive calls must not share backing storage.
	a := b.TopN(model.Ask, 3)
	c := b.TopN(model.Ask, 3)
	a[0] = model.Level{}
	if c[0].Price != mustPrice(t, "101.0") {
		t.Fatal("successive TopN calls share a backing array")
	}
}

func TestReadyLifecycle(t *testing.T) {
	b := New()
	if b.Ready() {
		t.Fatal("new book must not be ready")
	}
	// Updates before a snapshot are the engine's job to skip, but ready stays false.
	b.ApplySnapshot([]model.Level{lv(t, "1.0", "1.0")}, []model.Level{lv(t, "2.0", "1.0")})
	if !b.Ready() {
		t.Fatal("book must be ready after a snapshot")
	}
	b.Invalidate()
	if b.Ready() || b.Depth(model.Bid) != 0 || b.Depth(model.Ask) != 0 {
		t.Fatal("Invalidate must clear the book and mark not-ready")
	}
}

func TestEmptySideBest(t *testing.T) {
	b := New()
	if _, ok := b.BestBid(); ok {
		t.Fatal("BestBid on empty book must return ok=false")
	}
	if _, ok := b.BestAsk(); ok {
		t.Fatal("BestAsk on empty book must return ok=false")
	}
	if b.IsCrossedOrLocked() {
		t.Fatal("empty book is not crossed")
	}
}
