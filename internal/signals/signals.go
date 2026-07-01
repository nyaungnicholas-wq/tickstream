// Package signals computes the top-of-book microstructure signals that ship
// LIVE in v1: order-book imbalance and the size-weighted mid (spec §8).
// The Stoikov micro-price is EXPLAINED in docs/microprice.md and implemented
// only as an M6 stretch goal — be honest about which is which.
//
// This is the only layer that converts the fixed-point book values to
// float64; the book itself stays exact.
package signals

import "github.com/nyaungnicholas-wq/tickstream/internal/model"

// Imbalance returns both order-book imbalance conventions (state yours
// explicitly — two exist):
//
//	signed   = (Qb-Qa)/(Qb+Qa)  in [-1,+1]; > 0 = buy pressure
//	fraction = Qb/(Qb+Qa)       in [0,1]   (Stoikov's "imbalance")
//
// Identity (✅ verified): signed == 2*fraction - 1.
// With no size on either side it returns (0, 0.5) — a defined neutral.
func Imbalance(qb, qa float64) (signed, fraction float64) {
	total := qb + qa
	if total == 0 {
		return 0, 0.5
	}
	return (qb - qa) / total, qb / total
}

// WeightedMid is the size-weighted mid ("weighted mid-price"):
//
//	wmid = (Pb*Qa + Pa*Qb) / (Qa + Qb)
//
// CROSS-multiply — each side's PRICE is weighted by the OPPOSITE side's
// SIZE, so a heavy bid pushes fair value UP toward the ask. Multiplying
// same-side (Pb*Qb + Pa*Qa) is a DIFFERENT quantity (a book VWAP), wrong as
// a fair-value estimator.
//
// Honest caveat (README §7): the weighted mid is NOT a martingale and is
// noisy — Stoikov's example shows it moving down when an ask is merely
// cancelled. It is a cheap one-line imbalance-aware mid, not ground truth.
// With no size on either side it falls back to the plain mid.
func WeightedMid(pb, pa, qb, qa float64) float64 {
	total := qa + qb
	if total == 0 {
		return Mid(pb, pa)
	}
	return (pb*qa + pa*qb) / total
}

// Mid is the plain mid-price (Pb+Pa)/2.
func Mid(pb, pa float64) float64 { return (pb + pa) / 2 }

// DepthWeightedImbalance is the equal-weight depth-N signed imbalance
// (spec §8.1). Distance/decay weighting schemes are modeling choices, not
// canonical — this uses equal weights. Returns 0 for empty inputs.
func DepthWeightedImbalance(bids, asks []model.Level, n int) float64 {
	var qb, qa float64
	for i, lv := range bids {
		if i == n {
			break
		}
		qb += lv.Qty.Float64()
	}
	for i, lv := range asks {
		if i == n {
			break
		}
		qa += lv.Qty.Float64()
	}
	signed, _ := Imbalance(qb, qa)
	return signed
}
