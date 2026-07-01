package signals

import (
	"math"
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// The ✅-verified identity: signed == 2*fraction - 1, across a table.
func TestImbalanceIdentity(t *testing.T) {
	tests := []struct{ qb, qa float64 }{
		{1, 1}, {3, 1}, {1, 3}, {0.001, 42}, {7.5, 0}, {0, 7.5},
	}
	for _, tt := range tests {
		signed, fraction := Imbalance(tt.qb, tt.qa)
		if !almostEqual(signed, 2*fraction-1) {
			t.Fatalf("Imbalance(%v,%v): signed=%v, 2*fraction-1=%v", tt.qb, tt.qa, signed, 2*fraction-1)
		}
		if signed < -1 || signed > 1 || fraction < 0 || fraction > 1 {
			t.Fatalf("Imbalance(%v,%v) out of range: %v, %v", tt.qb, tt.qa, signed, fraction)
		}
	}
}

func TestImbalanceHandComputed(t *testing.T) {
	signed, fraction := Imbalance(3, 1)
	if !almostEqual(signed, 0.5) || !almostEqual(fraction, 0.75) {
		t.Fatalf("Imbalance(3,1) = %v, %v; want 0.5, 0.75", signed, fraction)
	}
	signed, fraction = Imbalance(0, 0)
	if signed != 0 || fraction != 0.5 {
		t.Fatalf("Imbalance(0,0) = %v, %v; want neutral 0, 0.5", signed, fraction)
	}
}

// The ✅-verified direction: with Qb > Qa (buy pressure) the weighted mid
// moves TOWARD THE ASK (up) — the M4 acceptance criterion.
func TestWeightedMidMovesTowardAskOnBuyPressure(t *testing.T) {
	pb, pa := 100.0, 101.0
	mid := Mid(pb, pa)

	up := WeightedMid(pb, pa, 3, 1) // heavy bid
	if !(up > mid && up <= pa) {
		t.Fatalf("wmid(Qb>Qa) = %v; want in (mid=%v, ask=%v]", up, mid, pa)
	}
	down := WeightedMid(pb, pa, 1, 3) // heavy ask
	if !(down < mid && down >= pb) {
		t.Fatalf("wmid(Qb<Qa) = %v; want in [bid=%v, mid=%v)", down, pb, mid)
	}
	balanced := WeightedMid(pb, pa, 2, 2)
	if !almostEqual(balanced, mid) {
		t.Fatalf("wmid(balanced) = %v, want mid %v", balanced, mid)
	}
}

// Hand-computed: Pb=100 Pa=101 Qb=3 Qa=1 -> (100*1 + 101*3)/4 = 100.75.
func TestWeightedMidHandComputed(t *testing.T) {
	if got := WeightedMid(100, 101, 3, 1); !almostEqual(got, 100.75) {
		t.Fatalf("WeightedMid = %v, want 100.75", got)
	}
}

// Algebraic equivalence with the imbalance form: wmid == I*Pa + (1-I)*Pb.
func TestWeightedMidImbalanceForm(t *testing.T) {
	pb, pa, qb, qa := 99.5, 100.1, 4.2, 1.7
	_, frac := Imbalance(qb, qa)
	want := frac*pa + (1-frac)*pb
	if got := WeightedMid(pb, pa, qb, qa); !almostEqual(got, want) {
		t.Fatalf("WeightedMid = %v, imbalance form = %v", got, want)
	}
}

func TestDepthWeightedImbalance(t *testing.T) {
	mk := func(q string) model.Level {
		v, err := model.QtyFromString(q)
		if err != nil {
			t.Fatal(err)
		}
		return model.Level{Qty: v}
	}
	bids := []model.Level{mk("3.0"), mk("1.0")}
	asks := []model.Level{mk("1.0"), mk("1.0")}
	// depth 2: (4-2)/(4+2) = 1/3
	if got := DepthWeightedImbalance(bids, asks, 2); !almostEqual(got, 1.0/3.0) {
		t.Fatalf("depth-2 imbalance = %v, want 1/3", got)
	}
	// depth 1: (3-1)/(3+1) = 0.5
	if got := DepthWeightedImbalance(bids, asks, 1); !almostEqual(got, 0.5) {
		t.Fatalf("depth-1 imbalance = %v, want 0.5", got)
	}
}
