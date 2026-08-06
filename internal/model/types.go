// Package model defines the shared value types used across TickStream.
//
// Prices and quantities are fixed-point scaled int64 (Scale = 1e8), never
// float64: order books must be exact, and the Kraken checksum additionally
// requires the exact decimal string as delivered on the wire (see Level).
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Side is the side of the book a level belongs to.
type Side uint8

// Book sides.
const (
	Bid Side = iota
	Ask
)

func (s Side) String() string {
	switch s {
	case Bid:
		return "bid"
	case Ask:
		return "ask"
	default:
		return fmt.Sprintf("Side(%d)", uint8(s))
	}
}

// Venue identifies an exchange feed.
type Venue uint8

// Supported venues.
const (
	Coinbase Venue = iota
	Kraken
)

func (v Venue) String() string {
	switch v {
	case Coinbase:
		return "coinbase"
	case Kraken:
		return "kraken"
	default:
		return fmt.Sprintf("Venue(%d)", uint8(v))
	}
}

// EventKind distinguishes a full-book snapshot from an incremental update.
//
// NOTE: the constants are prefixed Kind* because the unprefixed name
// "Snapshot" is taken by the Snapshot struct in this package (Go does not
// allow a constant and a type to share a name in one package).
type EventKind uint8

// Event kinds.
const (
	KindSnapshot EventKind = iota
	KindUpdate
	// KindTrade is an execution print off the venue's public trade channel.
	// It carries Trades and no book levels: the engine ignores it for book
	// state, but the tape needs it — realized VWAP and market impact cannot
	// be measured from the book alone.
	KindTrade
)

func (k EventKind) String() string {
	switch k {
	case KindSnapshot:
		return "snapshot"
	case KindUpdate:
		return "update"
	case KindTrade:
		return "trade"
	default:
		return fmt.Sprintf("EventKind(%d)", uint8(k))
	}
}

// Scale is the fixed-point denominator for Price and Qty: 8 decimal places.
const Scale = 100_000_000

// scaleDigits is the number of fractional decimal digits Scale represents.
const scaleDigits = 8

// Price is a fixed-point price: the real price multiplied by Scale.
type Price int64

// Qty is a fixed-point quantity: the real quantity multiplied by Scale.
type Qty int64

var errBadDecimal = errors.New("model: invalid decimal string")

// parseFixed parses a non-negative decimal string ("10101.10", "0.001", "45285.2")
// into a Scale-scaled int64 using integer math only. It never round-trips
// through float64 (which cannot represent most decimal fractions exactly).
// More than 8 fractional digits is an error unless the excess digits are zero.
func parseFixed(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", errBadDecimal)
	}
	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, fmt.Errorf("%w: %q", errBadDecimal, s)
		}
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("%w: %q", errBadDecimal, s)
	}
	var n int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", errBadDecimal, s)
		}
		d := int64(c - '0')
		if n > (1<<63-1-d)/10 {
			return 0, fmt.Errorf("%w: overflow in %q", errBadDecimal, s)
		}
		n = n*10 + d
	}
	if n > (1<<63-1)/Scale {
		return 0, fmt.Errorf("%w: overflow in %q", errBadDecimal, s)
	}
	n *= Scale
	for i := 0; i < len(fracPart); i++ {
		c := fracPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", errBadDecimal, s)
		}
		if i >= scaleDigits {
			if c != '0' {
				return 0, fmt.Errorf("%w: more than %d fractional digits in %q", errBadDecimal, scaleDigits, s)
			}
			continue
		}
		// digit i contributes d * 10^(scaleDigits-1-i)
		mul := int64(1)
		for p := 0; p < scaleDigits-1-i; p++ {
			mul *= 10
		}
		n += int64(c-'0') * mul
	}
	return n, nil
}

// PriceFromString parses an exact decimal price string into a Price.
func PriceFromString(s string) (Price, error) {
	n, err := parseFixed(s)
	return Price(n), err
}

// QtyFromString parses an exact decimal quantity string into a Qty.
func QtyFromString(s string) (Qty, error) {
	n, err := parseFixed(s)
	return Qty(n), err
}

// formatFixed renders a scaled int64 back to a decimal string, trimming
// trailing fractional zeros ("10101.1", "0.001", "45285.2", "0").
func formatFixed(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	whole, frac := n/Scale, n%Scale
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d", whole)
	if frac != 0 {
		fs := fmt.Sprintf("%08d", frac)
		fs = strings.TrimRight(fs, "0")
		b.WriteByte('.')
		b.WriteString(fs)
	}
	return b.String()
}

func (p Price) String() string { return formatFixed(int64(p)) }

func (q Qty) String() string { return formatFixed(int64(q)) }

// Float64 converts a Price to float64. Signal-layer use only (§8); the book
// itself stays fixed-point.
func (p Price) Float64() float64 { return float64(p) / Scale }

// Float64 converts a Qty to float64. Signal-layer use only.
func (q Qty) Float64() float64 { return float64(q) / Scale }

// Level is one price level. PriceStr and QtyStr preserve the exact decimal
// strings as delivered by the venue — the Kraken CRC32 checksum (§6.5) is
// computed over those verbatim digits, so they must never be re-derived from
// the scaled ints.
type Level struct {
	Price    Price
	Qty      Qty
	PriceStr string
	QtyStr   string
}

// Trade is one public execution print, normalized across venues.
//
// Aggressor is the side that TOOK liquidity, which the venues report
// differently and inconsistently: Coinbase Exchange `match.side` is the
// MAKER's side and must be inverted; Kraken v2 `trade.side` is already the
// taker's. Normalizing here means downstream code never has to remember
// which is which. Bid means a buyer lifted the offer.
type Trade struct {
	Price     Price
	Qty       Qty
	PriceStr  string
	QtyStr    string
	Aggressor Side
	TimeNanos int64
	ID        string
}

// Event is a normalized book event from a venue feed.
type Event struct {
	Venue      Venue
	Kind       EventKind
	Symbol     string
	Bids, Asks []Level
	// Trades is populated only on KindTrade events; a venue may batch
	// several prints into one frame.
	Trades         []Trade
	Checksum       uint32 // Kraken only; 0 otherwise
	EventTimeNanos int64  // venue event time if provided, else 0
	// Received is the monotonic event-received timestamp, stamped by the
	// feed goroutine just after the websocket read returns (spec §9.1: the
	// apply-latency headline starts HERE, so queueing on the bounded
	// hand-off channel is included). Zero for fixture-driven tests.
	// Stored as time.Time because Go's monotonic reading only survives
	// inside a time.Time; time.Since(Received) is NTP-step-immune.
	Received time.Time
}

// VenueTop is one venue's top-of-book as published in a Snapshot.
// Bids/Asks carry the top-N depth (best-first) for readers that render
// ladders; like everything in a Snapshot they are FRESH slices per publish
// (§4.3) — TopN already returns fresh backing storage.
type VenueTop struct {
	Venue            Venue
	Ready            bool
	HasBid, HasAsk   bool
	BestBid, BestAsk Level
	Bids, Asks       []Level
}

// ConsolidatedBBO is the cross-venue best bid/offer (crypto "NBBO").
type ConsolidatedBBO struct {
	BestBidPrice, BestAskPrice Price
	BestBidSize, BestAskSize   Qty
	BestBidVenue, BestAskVenue Venue
	Spread, Mid                Price
	CrossVenueCrossed          bool
	Valid                      bool // false until >=1 ready venue with both sides
}

// Snapshot is the immutable value published by the engine after every apply.
//
// IMMUTABILITY INVARIANT (§4.3): a published *Snapshot — and every slice and
// map it transitively owns — is immutable and freshly allocated per publish.
// The engine must never retain a reference into a published snapshot nor reuse
// a backing array/map across publishes. Readers may hold an old *Snapshot
// indefinitely; it must never change under them. This is a logic race that
// `go test -race` cannot detect — the concurrent test in internal/snapshot
// guards it.
type Snapshot struct {
	Venues            map[Venue]VenueTop // fresh map each publish
	Consolidated      ConsolidatedBBO
	ImbalanceSigned   float64
	ImbalanceFraction float64
	WeightedMid       float64
	Mid               float64
	SpreadTicks       Price
	CrossVenueCrossed bool
	PublishUnixNanos  int64 // when this snapshot was published
	ApplyLatencyNanos int64 // event-received -> published (engine, M3+)
}
