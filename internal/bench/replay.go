package bench

import (
	"fmt"

	"github.com/nyaungnicholas-wq/tickstream/internal/checksum"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// generateEvents builds a deterministic replayed Kraken feed: one snapshot
// establishing a 10-level book, then updates that overwrite one level's qty
// at a time, each carrying the CORRECT CRC32 for the resulting book — so the
// benchmark exercises the full real apply path (book apply, truncate, crossed
// guard, checksum verify, fresh-snapshot publish) without ever resyncing.
// All generation happens BEFORE the timed loop; nothing here is measured.
func generateEvents(n int) []model.Event {
	// qtyPool cycles realistic decimal strings; parsed once.
	qtyPool := []string{
		"0.10000000", "1.54582015", "0.07990000", "2.00000000",
		"0.33100103", "5.12345678", "0.00100000", "3.14159265",
		"0.90000000", "1.00000001", "0.25000000", "7.77777777",
	}
	type slot struct{ price, qty string }
	bids := make([]slot, 10) // high -> low (Kraken snapshot order)
	asks := make([]slot, 10) // low -> high
	for i := 0; i < 10; i++ {
		bids[i] = slot{fmt.Sprintf("59999.%d", 9-i), qtyPool[i%len(qtyPool)]}
		asks[i] = slot{fmt.Sprintf("60000.%d", i), qtyPool[(i+5)%len(qtyPool)]}
	}

	mkLevel := func(s slot) model.Level {
		p, err := model.PriceFromString(s.price)
		if err != nil {
			panic(err) // static generator data; unreachable
		}
		q, err := model.QtyFromString(s.qty)
		if err != nil {
			panic(err)
		}
		return model.Level{Price: p, Qty: q, PriceStr: s.price, QtyStr: s.qty}
	}
	crc := func() uint32 {
		pairs := func(ss []slot) [][2]string {
			out := make([][2]string, len(ss))
			for i, s := range ss {
				out[i] = [2]string{s.price, s.qty}
			}
			return out
		}
		return checksum.BookChecksum(pairs(asks), pairs(bids))
	}

	events := make([]model.Event, 0, n)

	// Event 0: the snapshot.
	snap := model.Event{Venue: model.Kraken, Kind: model.KindSnapshot, Symbol: "BTC/USD", Checksum: crc()}
	for _, s := range bids {
		snap.Bids = append(snap.Bids, mkLevel(s))
	}
	for _, s := range asks {
		snap.Asks = append(snap.Asks, mkLevel(s))
	}
	events = append(events, snap)

	// Updates: deterministic level-qty overwrites alternating sides.
	for i := 1; i < n; i++ {
		side := i % 2
		idx := (i / 2) % 10
		newQty := qtyPool[(i*7)%len(qtyPool)]
		ev := model.Event{Venue: model.Kraken, Kind: model.KindUpdate, Symbol: "BTC/USD"}
		if side == 0 {
			bids[idx].qty = newQty
			ev.Bids = []model.Level{mkLevel(bids[idx])}
		} else {
			asks[idx].qty = newQty
			ev.Asks = []model.Level{mkLevel(asks[idx])}
		}
		ev.Checksum = crc()
		events = append(events, ev)
	}
	return events
}
