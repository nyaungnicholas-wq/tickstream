// Package execcost prices what it actually costs to execute a market order
// against real cross-venue order-book depth.
//
// THE CAVEAT THAT GOVERNS EVERY NUMBER THIS PACKAGE PRODUCES: it measures cost
// against DISPLAYED depth, which is a LOWER BOUND on real cost. Hidden and
// iceberg size is invisible in L2, and market makers pull quotes when they see
// aggression, so a real order of the same size would generally pay more than
// Walk reports — never less. Any figure derived from this package must be
// reported as a floor, not an estimate.
//
// All money arithmetic is exact fixed-point. Price and Qty are both scaled by
// 1e8, so their product overflows int64 at ordinary BTC prices; every multiply
// goes through a 128-bit intermediate (see mulDiv). A bare int64 multiply of
// two scaled quantities anywhere in this package is a bug.
package execcost

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// Sentinel errors, checkable with errors.Is.
var (
	// ErrOverflow means a fixed-point intermediate would not fit its result.
	ErrOverflow = errors.New("execcost: fixed-point overflow")
	// ErrEmptyLadder means there is no displayed depth to walk.
	ErrEmptyLadder = errors.New("execcost: empty ladder")
	// ErrNonPositiveQty means the requested size was zero or negative.
	ErrNonPositiveQty = errors.New("execcost: non-positive quantity")
	// ErrInsufficientDepth means the request exceeds total displayed depth.
	ErrInsufficientDepth = errors.New("execcost: insufficient displayed depth")
	// ErrUnsorted means the ladder was not sorted best-first, which would
	// silently misprice the walk.
	ErrUnsorted = errors.New("execcost: ladder not sorted best-first")
)

// maxInt64 is the largest value an int64 can hold.
const maxInt64 = int64(^uint64(0) >> 1)

// Notional is a USD amount in the same 1e8 fixed-point scale as model.Price.
type Notional int64

// Float64 renders the notional as USD. For display and charting only — never
// feed it back into the money math.
func (n Notional) Float64() float64 { return float64(n) / float64(model.Scale) }

// String renders the notional as a plain decimal USD string.
func (n Notional) String() string {
	neg, v := "", int64(n)
	if v < 0 {
		neg, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%08d", neg, v/model.Scale, v%model.Scale)
}

// VenueLevel is one resting price level tagged with the venue holding it.
type VenueLevel struct {
	Venue model.Venue
	Price model.Price
	Qty   model.Qty
}

// Ladder is one side of the consolidated book, sorted BEST FIRST: asks
// ascending by price, bids descending.
type Ladder []VenueLevel

// Fill is one level's worth of an executed walk.
type Fill struct {
	Venue model.Venue
	Price model.Price
	Qty   model.Qty
}

// Cost is the result of walking a ladder for a given size.
type Cost struct {
	Side       model.Side
	Requested  model.Qty
	Filled     model.Qty
	Notional   Notional
	AvgPrice   model.Price
	TouchPrice model.Price
	// SlippageBps is signed so POSITIVE always means "worse than the touch"
	// on both sides: a buy paying above the best ask and a sell hitting below
	// the best bid both report positive.
	SlippageBps float64
	Levels      int
	Fills       []Fill
	PerVenue    map[model.Venue]model.Qty
}

// mulDiv computes a*b/denom through a 128-bit intermediate.
//
// bits.Div64 PANICS unless hi < denom, so the guard below is not defensive
// padding — it is what keeps this from crashing on ordinary BTC-sized inputs.
func mulDiv(a, b, denom uint64) (uint64, error) {
	if denom == 0 {
		return 0, ErrOverflow
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= denom {
		return 0, ErrOverflow
	}
	q, _ := bits.Div64(hi, lo, denom)
	if q > uint64(maxInt64) {
		return 0, ErrOverflow
	}
	return q, nil
}

// Merge builds a consolidated one-side ladder from per-venue level slices.
//
// Levels at the SAME price on DIFFERENT venues stay as separate entries: both
// are independently takeable and each carries its own venue's fee. Ties break
// by descending size, then venue order — L2 carries no per-order timestamps,
// so true time priority is unknowable here and venue order is only a
// deterministic stand-in.
//
// Levels with non-positive size are deletions, not liquidity, and are skipped.
func Merge(side model.Side, per map[model.Venue][]model.Level) Ladder {
	var ladder Ladder
	for venue, levels := range per {
		for _, lv := range levels {
			if lv.Qty <= 0 || lv.Price <= 0 {
				continue
			}
			ladder = append(ladder, VenueLevel{Venue: venue, Price: lv.Price, Qty: lv.Qty})
		}
	}
	if len(ladder) == 0 {
		return nil
	}
	sort.Slice(ladder, func(i, j int) bool {
		a, b := ladder[i], ladder[j]
		if a.Price != b.Price {
			if side == model.Ask {
				return a.Price < b.Price // cheapest offer first
			}
			return a.Price > b.Price // highest bid first
		}
		if a.Qty != b.Qty {
			return a.Qty > b.Qty
		}
		return a.Venue < b.Venue
	})
	return ladder
}

// TotalDepth is the sum of displayed size across the ladder.
func TotalDepth(l Ladder) model.Qty {
	var total model.Qty
	for _, lv := range l {
		total += lv.Qty
	}
	return total
}

// Walk consumes the ladder best-first until q is filled and reports the cost.
//
// It REFUSES rather than extrapolating past the displayed depth: returning a
// price for size the book cannot supply would look like a measurement. On
// ErrInsufficientDepth the returned Cost still carries what actually filled,
// so the caller can see how close it came.
func Walk(l Ladder, side model.Side, q model.Qty) (Cost, error) {
	if q <= 0 {
		return Cost{}, ErrNonPositiveQty
	}
	if len(l) == 0 {
		return Cost{}, ErrEmptyLadder
	}
	for i := 1; i < len(l); i++ {
		if side == model.Ask && l[i].Price < l[i-1].Price {
			return Cost{}, ErrUnsorted
		}
		if side == model.Bid && l[i].Price > l[i-1].Price {
			return Cost{}, ErrUnsorted
		}
	}

	cost := Cost{Side: side, Requested: q, TouchPrice: l[0].Price}
	remaining := q
	var notional int64
	perVenue := make(map[model.Venue]model.Qty)

	for _, lv := range l {
		if remaining <= 0 {
			break
		}
		take := lv.Qty
		if take > remaining {
			take = remaining
		}
		n, err := mulDiv(uint64(lv.Price), uint64(take), uint64(model.Scale))
		if err != nil {
			return cost, err
		}
		// Guard the running total too: each level is individually bounded,
		// but a deep enough walk could still carry the sum past int64.
		if notional > maxInt64-int64(n) {
			return cost, ErrOverflow
		}
		notional += int64(n)
		cost.Filled += take
		cost.Fills = append(cost.Fills, Fill{Venue: lv.Venue, Price: lv.Price, Qty: take})
		perVenue[lv.Venue] += take
		remaining -= take
	}

	cost.Notional = Notional(notional)
	cost.Levels = len(cost.Fills)
	if len(cost.Fills) > 0 {
		cost.PerVenue = perVenue
	}

	// Report the average and slippage of whatever DID fill, even on a refusal:
	// "only 3.2 of 10 BTC available, and that 3.2 cost 14bps" is a far more
	// useful refusal than a bare error.
	if cost.Filled > 0 {
		avg, err := mulDiv(uint64(notional), uint64(model.Scale), uint64(cost.Filled))
		if err != nil {
			return cost, err
		}
		cost.AvgPrice = model.Price(avg)
		cost.SlippageBps = slippageBps(side, cost.TouchPrice, cost.AvgPrice)
	}
	if cost.Filled < cost.Requested {
		return cost, ErrInsufficientDepth
	}
	return cost, nil
}

// slippageBps returns the signed cost versus the touch; positive == worse.
func slippageBps(side model.Side, touch, avg model.Price) float64 {
	if touch == 0 {
		return math.NaN()
	}
	diff := int64(avg) - int64(touch) // a buy pays UP from the best ask
	if side == model.Bid {
		diff = int64(touch) - int64(avg) // a sell receives DOWN from the best bid
	}
	return float64(diff) * 10000.0 / float64(touch)
}
