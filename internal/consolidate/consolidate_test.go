package consolidate

import (
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

func top(t *testing.T, venue model.Venue, bidPx, bidQty, askPx, askQty string) model.VenueTop {
	t.Helper()
	mk := func(px, qty string) model.Level {
		p, err := model.PriceFromString(px)
		if err != nil {
			t.Fatal(err)
		}
		q, err := model.QtyFromString(qty)
		if err != nil {
			t.Fatal(err)
		}
		return model.Level{Price: p, Qty: q, PriceStr: px, QtyStr: qty}
	}
	return model.VenueTop{
		Venue: venue, Ready: true, HasBid: true, HasAsk: true,
		BestBid: mk(bidPx, bidQty), BestAsk: mk(askPx, askQty),
	}
}

func px(t *testing.T, s string) model.Price {
	t.Helper()
	p, err := model.PriceFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func qty(t *testing.T, s string) model.Qty {
	t.Helper()
	q, err := model.QtyFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestConsolidate(t *testing.T) {
	tests := []struct {
		name  string
		in    map[model.Venue]model.VenueTop
		check func(t *testing.T, got model.ConsolidatedBBO)
	}{
		{
			name: "best bid from venue A, best ask from venue B",
			in: map[model.Venue]model.VenueTop{
				model.Coinbase: top(t, model.Coinbase, "100.5", "1.0", "101.5", "1.0"),
				model.Kraken:   top(t, model.Kraken, "100.4", "2.0", "101.2", "2.0"),
			},
			check: func(t *testing.T, got model.ConsolidatedBBO) {
				if !got.Valid {
					t.Fatal("want Valid")
				}
				if got.BestBidPrice != px(t, "100.5") || got.BestBidVenue != model.Coinbase {
					t.Fatalf("best bid %v@%v, want 100.5@coinbase", got.BestBidPrice, got.BestBidVenue)
				}
				if got.BestAskPrice != px(t, "101.2") || got.BestAskVenue != model.Kraken {
					t.Fatalf("best ask %v@%v, want 101.2@kraken", got.BestAskPrice, got.BestAskVenue)
				}
				if got.Spread != px(t, "0.7") {
					t.Fatalf("spread = %v, want 0.7", got.Spread)
				}
				if got.CrossVenueCrossed {
					t.Fatal("not crossed")
				}
			},
		},
		{
			name: "same best price aggregates size across venues",
			in: map[model.Venue]model.VenueTop{
				model.Coinbase: top(t, model.Coinbase, "100.5", "1.0", "101.0", "1.5"),
				model.Kraken:   top(t, model.Kraken, "100.5", "2.0", "101.0", "0.5"),
			},
			check: func(t *testing.T, got model.ConsolidatedBBO) {
				if got.BestBidSize != qty(t, "3.0") {
					t.Fatalf("bid size = %v, want 3.0 (aggregated)", got.BestBidSize)
				}
				if got.BestAskSize != qty(t, "2.0") {
					t.Fatalf("ask size = %v, want 2.0 (aggregated)", got.BestAskSize)
				}
				// Tie-break by size: Kraken has the bigger bid, Coinbase the bigger ask.
				if got.BestBidVenue != model.Kraken || got.BestAskVenue != model.Coinbase {
					t.Fatalf("attribution = %v/%v, want kraken/coinbase (size tie-break)",
						got.BestBidVenue, got.BestAskVenue)
				}
			},
		},
		{
			name: "cross-venue crossed sets the flag (normal, no SIP)",
			in: map[model.Venue]model.VenueTop{
				// Coinbase bid 101.0 above Kraken ask 100.8 — each venue
				// individually uncrossed.
				model.Coinbase: top(t, model.Coinbase, "101.0", "1.0", "101.5", "1.0"),
				model.Kraken:   top(t, model.Kraken, "100.5", "1.0", "100.8", "1.0"),
			},
			check: func(t *testing.T, got model.ConsolidatedBBO) {
				if !got.CrossVenueCrossed {
					t.Fatal("want CrossVenueCrossed=true")
				}
				if got.BestBidPrice != px(t, "101.0") || got.BestAskPrice != px(t, "100.8") {
					t.Fatalf("bbo = %v/%v", got.BestBidPrice, got.BestAskPrice)
				}
				if got.Spread >= 0 {
					t.Fatalf("crossed spread should be negative, got %v", got.Spread)
				}
			},
		},
		{
			name: "single ready venue degenerates to its own BBO",
			in: map[model.Venue]model.VenueTop{
				model.Kraken: top(t, model.Kraken, "100.5", "1.0", "101.0", "2.0"),
			},
			check: func(t *testing.T, got model.ConsolidatedBBO) {
				if !got.Valid {
					t.Fatal("want Valid with one ready venue")
				}
				if got.BestBidVenue != model.Kraken || got.BestAskVenue != model.Kraken {
					t.Fatal("attribution must be the single venue")
				}
				if got.Mid != px(t, "100.75") {
					t.Fatalf("mid = %v, want 100.75", got.Mid)
				}
			},
		},
		{
			name: "not-ready and one-sided venues are ignored",
			in: map[model.Venue]model.VenueTop{
				model.Coinbase: {Venue: model.Coinbase, Ready: false},
				model.Kraken:   {Venue: model.Kraken, Ready: true, HasBid: true, HasAsk: false},
			},
			check: func(t *testing.T, got model.ConsolidatedBBO) {
				if got.Valid {
					t.Fatal("no quoting venue -> Valid must be false")
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, Consolidate(tt.in))
		})
	}
}
