// Package consolidate merges per-venue tops into one consolidated BBO — a
// crypto "NBBO" (spec §7).
//
// HONESTY NOTE (this is the quant-facing differentiator): unlike the US
// equity NBBO there is no SIP in crypto — WE build the consolidated book from
// per-exchange feeds that arrive with independent latency and clock skew. So
// the consolidated book can momentarily appear locked or crossed ACROSS
// venues (best bid >= best ask from DIFFERENT venues). That is normal and
// short-lived: it is flagged (CrossVenueCrossed), never resynced. The
// opposite holds within one venue, where a persistent cross means the local
// book is corrupt (engine resyncs it — see internal/engine).
package consolidate

import "github.com/nyaungnicholas-wq/tickstream/internal/model"

// venueOrder is the deterministic iteration order. It also serves as the
// final tie-break: L2 price levels carry no per-order timestamps, so true
// time priority is unknowable here — we tie-break price > size > (stable
// venue order as a time proxy) and say so.
var venueOrder = []model.Venue{model.Coinbase, model.Kraken}

// Consolidate computes the cross-venue BBO over the READY venues that have
// both sides. With a single ready venue it degenerates to that venue's BBO.
func Consolidate(venues map[model.Venue]model.VenueTop) model.ConsolidatedBBO {
	var out model.ConsolidatedBBO
	for _, v := range venueOrder {
		vt, ok := venues[v]
		if !ok || !vt.Ready || !vt.HasBid || !vt.HasAsk {
			continue // resyncing or one-sided venues don't quote
		}
		if !out.Valid {
			out.Valid = true
			out.BestBidPrice, out.BestBidSize, out.BestBidVenue = vt.BestBid.Price, vt.BestBid.Qty, v
			out.BestAskPrice, out.BestAskSize, out.BestAskVenue = vt.BestAsk.Price, vt.BestAsk.Qty, v
			continue
		}
		// Best bid: MAX across venues; aggregate size at the shared best;
		// tie-break attribution by size, then stable venue order.
		switch {
		case vt.BestBid.Price > out.BestBidPrice:
			out.BestBidPrice, out.BestBidSize, out.BestBidVenue = vt.BestBid.Price, vt.BestBid.Qty, v
		case vt.BestBid.Price == out.BestBidPrice:
			if vt.BestBid.Qty > out.BestBidSize {
				out.BestBidVenue = v
			}
			out.BestBidSize += vt.BestBid.Qty
		}
		// Best ask: MIN across venues.
		switch {
		case vt.BestAsk.Price < out.BestAskPrice:
			out.BestAskPrice, out.BestAskSize, out.BestAskVenue = vt.BestAsk.Price, vt.BestAsk.Qty, v
		case vt.BestAsk.Price == out.BestAskPrice:
			if vt.BestAsk.Qty > out.BestAskSize {
				out.BestAskVenue = v
			}
			out.BestAskSize += vt.BestAsk.Qty
		}
	}
	if !out.Valid {
		return out
	}
	out.Spread = out.BestAskPrice - out.BestBidPrice
	out.Mid = (out.BestBidPrice + out.BestAskPrice) / 2 // fixed-point; truncates the half-tick
	// Cross-venue locked/crossed: NORMAL and transient (no SIP) — flag only.
	// Each single-venue book is uncrossed (engine guarantees it), so a
	// consolidated cross can only arise across venues.
	out.CrossVenueCrossed = out.BestBidPrice >= out.BestAskPrice
	return out
}
