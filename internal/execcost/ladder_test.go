package execcost

import (
	"errors"
	"math"
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

func px(usd float64) model.Price { return model.Price(int64(usd * float64(model.Scale))) }
func qt(u float64) model.Qty     { return model.Qty(int64(u * float64(model.Scale))) }

// TestNotionalDoesNotOverflowAtBTCScale is the reason this package uses a
// 128-bit intermediate at all. Price and Qty are BOTH scaled by 1e8, so
// $64,000 x 1 BTC is 6.4e20 as a raw product — 69x past the int64 ceiling.
// A naive int64 multiply here wraps silently and reports a negative cost.
func TestNotionalDoesNotOverflowAtBTCScale(t *testing.T) {
	raw := int64(px(64000)) * int64(qt(1)) // what the naive version would do
	if raw > 0 {
		t.Fatalf("premise broken: expected the naive product to wrap, got %d", raw)
	}

	l := Ladder{{Venue: model.Kraken, Price: px(64000), Qty: qt(1)}}
	c, err := Walk(l, model.Ask, qt(1))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got, want := c.Notional, Notional(px(64000)); got != want {
		t.Fatalf("notional: got %s want %s", got, want)
	}
	if got := c.Notional.Float64(); math.Abs(got-64000) > 1e-6 {
		t.Fatalf("notional float: got %v want 64000", got)
	}
	if c.AvgPrice != px(64000) {
		t.Fatalf("avg price: got %v want %v", c.AvgPrice, px(64000))
	}
}

func TestWalkAcrossLevelsAndVenues(t *testing.T) {
	// 1 BTC @ 64000 (KR), 2 BTC @ 64010 (CB); buy 2 BTC.
	l := Ladder{
		{Venue: model.Kraken, Price: px(64000), Qty: qt(1)},
		{Venue: model.Coinbase, Price: px(64010), Qty: qt(2)},
	}
	c, err := Walk(l, model.Ask, qt(2))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if c.Filled != qt(2) {
		t.Fatalf("filled: got %v want %v", c.Filled, qt(2))
	}
	if c.Levels != 2 {
		t.Fatalf("levels: got %d want 2", c.Levels)
	}
	// 64000*1 + 64010*1 = 128010 -> avg 64005
	if want := Notional(px(128010)); c.Notional != want {
		t.Fatalf("notional: got %s want %s", c.Notional, want)
	}
	if c.AvgPrice != px(64005) {
		t.Fatalf("avg: got %v want %v", c.AvgPrice, px(64005))
	}
	if c.PerVenue[model.Kraken] != qt(1) || c.PerVenue[model.Coinbase] != qt(1) {
		t.Fatalf("per-venue split wrong: %+v", c.PerVenue)
	}
	// avg is 5/64000 above the touch = 0.781 bps
	if math.Abs(c.SlippageBps-0.78125) > 1e-6 {
		t.Fatalf("slippage: got %v want ~0.78125", c.SlippageBps)
	}
}

func TestWalkRefusesPastDisplayedDepth(t *testing.T) {
	l := Ladder{{Venue: model.Kraken, Price: px(64000), Qty: qt(0.5)}}
	c, err := Walk(l, model.Ask, qt(10))
	if !errors.Is(err, ErrInsufficientDepth) {
		t.Fatalf("want ErrInsufficientDepth, got %v", err)
	}
	// The refusal must still report how far it got — that is the useful part.
	if c.Filled != qt(0.5) {
		t.Fatalf("partial fill not reported: got %v", c.Filled)
	}
	if c.AvgPrice != px(64000) {
		t.Fatalf("partial avg not reported: got %v", c.AvgPrice)
	}
}

func TestSlippageSignConventionBothSides(t *testing.T) {
	// Buying walks UP the asks; selling walks DOWN the bids. Both must report
	// POSITIVE slippage, because both are worse than the touch.
	asks := Ladder{
		{Venue: model.Kraken, Price: px(100), Qty: qt(1)},
		{Venue: model.Kraken, Price: px(102), Qty: qt(1)},
	}
	buy, err := Walk(asks, model.Ask, qt(2))
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	bids := Ladder{
		{Venue: model.Kraken, Price: px(100), Qty: qt(1)},
		{Venue: model.Kraken, Price: px(98), Qty: qt(1)},
	}
	sell, err := Walk(bids, model.Bid, qt(2))
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if buy.SlippageBps <= 0 {
		t.Fatalf("buy slippage must be positive, got %v", buy.SlippageBps)
	}
	if sell.SlippageBps <= 0 {
		t.Fatalf("sell slippage must be positive, got %v", sell.SlippageBps)
	}
	if math.Abs(buy.SlippageBps-sell.SlippageBps) > 1e-9 {
		t.Fatalf("symmetric walks should cost the same: buy=%v sell=%v",
			buy.SlippageBps, sell.SlippageBps)
	}
}

func TestMergeOrdersAndKeepsSamePriceOnBothVenues(t *testing.T) {
	per := map[model.Venue][]model.Level{
		model.Coinbase: {
			{Price: px(64010), Qty: qt(1)},
			{Price: px(64000), Qty: qt(1)},
			{Price: px(64005), Qty: qt(0)}, // deletion, must be skipped
		},
		model.Kraken: {
			{Price: px(64000), Qty: qt(3)},
		},
	}
	asks := Merge(model.Ask, per)
	if len(asks) != 3 {
		t.Fatalf("want 3 levels (zero-qty skipped), got %d: %+v", len(asks), asks)
	}
	if asks[0].Price != px(64000) || asks[1].Price != px(64000) {
		t.Fatalf("same price on two venues must both survive: %+v", asks)
	}
	// Tie at 64000 breaks by descending size: Kraken's 3 BTC comes first.
	if asks[0].Venue != model.Kraken || asks[0].Qty != qt(3) {
		t.Fatalf("tie-break by size failed: %+v", asks[0])
	}
	if asks[2].Price != px(64010) {
		t.Fatalf("asks not ascending: %+v", asks)
	}

	bids := Merge(model.Bid, per)
	if bids[0].Price != px(64010) {
		t.Fatalf("bids must be descending, got %+v", bids)
	}
	if TotalDepth(asks) != qt(5) {
		t.Fatalf("total depth: got %v want %v", TotalDepth(asks), qt(5))
	}
}

func TestWalkRejectsBadInput(t *testing.T) {
	good := Ladder{{Venue: model.Kraken, Price: px(100), Qty: qt(1)}}
	for _, tc := range []struct {
		name string
		l    Ladder
		side model.Side
		q    model.Qty
		want error
	}{
		{"zero qty", good, model.Ask, 0, ErrNonPositiveQty},
		{"negative qty", good, model.Ask, -1, ErrNonPositiveQty},
		{"empty ladder", nil, model.Ask, qt(1), ErrEmptyLadder},
		{"asks descending", Ladder{
			{Price: px(102), Qty: qt(1)}, {Price: px(100), Qty: qt(1)},
		}, model.Ask, qt(1), ErrUnsorted},
		{"bids ascending", Ladder{
			{Price: px(100), Qty: qt(1)}, {Price: px(102), Qty: qt(1)},
		}, model.Bid, qt(1), ErrUnsorted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Walk(tc.l, tc.side, tc.q); !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestMulDivGuardsTheDiv64Panic(t *testing.T) {
	// hi >= denom would panic inside bits.Div64; it must return an error.
	if _, err := mulDiv(math.MaxUint64, math.MaxUint64, 1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
	if _, err := mulDiv(1, 1, 0); !errors.Is(err, ErrOverflow) {
		t.Fatalf("zero denominator must error, got %v", err)
	}
}
