package execcost

import (
	"fmt"
	"sort"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// Fees decide this study's conclusion. A cross-venue dislocation of a few
// dollars on a $60k asset is ~1bp; a single taker fee at the entry tier is
// ~80bp. Get the schedule wrong and every verdict flips, so each schedule
// carries its provenance and an explicit Verified flag, and the harness
// refuses to publish a headline number while any input is unverified.

// Tier is one 30-day-volume band of a venue's fee schedule. Rates are
// fractions, not percents: 0.008 is 80 basis points.
type Tier struct {
	MinVolumeUSD float64
	Maker        float64
	Taker        float64
}

// Schedule is one venue's published fee ladder plus where it came from.
type Schedule struct {
	Venue model.Venue
	// AsOf and Source record provenance. A fee table without a date is a
	// guess wearing a number's clothes.
	AsOf   string
	Source string
	// Verified is false when the numbers could not be read from the venue's
	// own published schedule. Callers must surface this, not silently use it.
	Verified bool
	// Note explains any caveat, and is printed alongside every result.
	Note  string
	Tiers []Tier
}

// KrakenSpot is Kraken's published spot schedule.
//
// Read from kraken.com/features/fee-schedule on 2026-08-06. Kraken assigns the
// tier on the BEST of 30-day volume or assets-on-platform; this table uses the
// volume column only, which is the conservative reading.
var KrakenSpot = Schedule{
	Venue:    model.Kraken,
	AsOf:     "2026-08-06",
	Source:   "https://www.kraken.com/features/fee-schedule",
	Verified: true,
	Tiers: []Tier{
		{0, 0.0040, 0.0080},
		{2_500, 0.0030, 0.0060},
		{10_000, 0.0022, 0.0038},
		{25_000, 0.0020, 0.0035},
		{50_000, 0.0015, 0.0030},
		{100_000, 0.0012, 0.0025},
		{250_000, 0.0010, 0.0022},
		{500_000, 0.0008, 0.0020},
		{1_000_000, 0.0006, 0.0018},
		{2_500_000, 0.0004, 0.0015},
		{5_000_000, 0.0002, 0.0012},
		{10_000_000, 0.0000, 0.0010},
	},
}

// CoinbaseExchange is Coinbase EXCHANGE's published schedule — the
// institutional product TickStream actually connects to, NOT Advanced Trade.
//
// Read from exchange.coinbase.com/fees on 2026-08-06. The distinction matters:
// Advanced Trade charges 1.20% taker at the entry tier, Exchange charges 0.60%,
// so using the wrong product's ladder doubles every Coinbase cost figure.
//
// Stable-pair rates (0.00% / 0.0045%) are not modelled; this study is BTC-USD.
var CoinbaseExchange = Schedule{
	Venue:    model.Coinbase,
	AsOf:     "2026-08-06",
	Source:   "https://exchange.coinbase.com/fees",
	Verified: true,
	Tiers: []Tier{
		{0, 0.0040, 0.0060},
		{10_000, 0.0025, 0.0040},
		{50_000, 0.0015, 0.0025},
		{100_000, 0.0010, 0.0020},
		{1_000_000, 0.0008, 0.0018},
		{15_000_000, 0.0006, 0.0016},
		{75_000_000, 0.0003, 0.0010},
		{250_000_000, 0.0000, 0.0006},
		{400_000_000, 0.0000, 0.0004},
	},
}

// FeeTable maps each venue to its schedule at a chosen 30-day volume.
type FeeTable struct {
	VolumeUSD float64
	schedules map[model.Venue]Schedule
}

// NewFeeTable builds a table for the given 30-day USD volume.
func NewFeeTable(volumeUSD float64, scheds ...Schedule) *FeeTable {
	m := make(map[model.Venue]Schedule, len(scheds))
	for _, s := range scheds {
		sort.Slice(s.Tiers, func(i, j int) bool {
			return s.Tiers[i].MinVolumeUSD < s.Tiers[j].MinVolumeUSD
		})
		m[s.Venue] = s
	}
	return &FeeTable{VolumeUSD: volumeUSD, schedules: m}
}

// DefaultFeeTable is the entry-tier table: a retail account doing no volume,
// which is the case the study is actually about.
func DefaultFeeTable() *FeeTable {
	return NewFeeTable(0, KrakenSpot, CoinbaseExchange)
}

// TakerRate returns the taker fee fraction for a venue, and whether the
// venue's schedule is verified.
func (f *FeeTable) TakerRate(v model.Venue) (rate float64, verified bool, err error) {
	s, ok := f.schedules[v]
	if !ok {
		return 0, false, fmt.Errorf("execcost: no fee schedule for venue %v", v)
	}
	t := s.tierFor(f.VolumeUSD)
	return t.Taker, s.Verified, nil
}

// MakerRate returns the maker fee fraction for a venue.
func (f *FeeTable) MakerRate(v model.Venue) (rate float64, verified bool, err error) {
	s, ok := f.schedules[v]
	if !ok {
		return 0, false, fmt.Errorf("execcost: no fee schedule for venue %v", v)
	}
	t := s.tierFor(f.VolumeUSD)
	return t.Maker, s.Verified, nil
}

// tierFor returns the highest tier whose threshold the volume reaches.
func (s Schedule) tierFor(volumeUSD float64) Tier {
	out := s.Tiers[0]
	for _, t := range s.Tiers {
		if volumeUSD >= t.MinVolumeUSD {
			out = t
		}
	}
	return out
}

// AllVerified reports whether every schedule in the table came from the
// venue's own published numbers. Callers must gate any headline claim on this.
func (f *FeeTable) AllVerified() bool {
	for _, s := range f.schedules {
		if !s.Verified {
			return false
		}
	}
	return true
}

// Caveats returns one line per unverified schedule, for printing alongside
// every result that used the table.
func (f *FeeTable) Caveats() []string {
	var out []string
	for _, s := range f.schedules {
		if !s.Verified {
			out = append(out, fmt.Sprintf("%s: %s (source: %s)", s.Venue, s.Note, s.Source))
		}
	}
	sort.Strings(out)
	return out
}

// TakerFee returns the taker fee payable on a walk, charged per venue on that
// venue's share of the notional — routing across venues with different fee
// tiers is exactly why this cannot be a single multiply on the total.
func (f *FeeTable) TakerFee(c Cost) (Notional, error) {
	var total Notional
	// Recompute per-venue notional from the fills; PerVenue holds quantity,
	// and quantity at different prices is not proportional to cost.
	perVenue := make(map[model.Venue]int64)
	for _, fl := range c.Fills {
		n, err := mulDiv(uint64(fl.Price), uint64(fl.Qty), uint64(model.Scale))
		if err != nil {
			return 0, err
		}
		perVenue[fl.Venue] += int64(n)
	}
	for v, notional := range perVenue {
		rate, _, err := f.TakerRate(v)
		if err != nil {
			return 0, err
		}
		total += Notional(float64(notional) * rate)
	}
	return total, nil
}

// TotalCost is the walk's notional plus taker fees — what actually leaves the
// account on a buy.
func (f *FeeTable) TotalCost(c Cost) (Notional, error) {
	fee, err := f.TakerFee(c)
	if err != nil {
		return 0, err
	}
	return c.Notional + fee, nil
}

// FeeBps expresses the taker fee on a walk in basis points of its notional,
// which is the unit that makes it comparable to the slippage figure.
func (f *FeeTable) FeeBps(c Cost) (float64, error) {
	if c.Notional == 0 {
		return 0, nil
	}
	fee, err := f.TakerFee(c)
	if err != nil {
		return 0, err
	}
	return float64(fee) * 10000.0 / float64(c.Notional), nil
}
