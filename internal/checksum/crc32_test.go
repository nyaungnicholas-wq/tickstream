package checksum

import "testing"

func TestFmtVal(t *testing.T) {
	tests := []struct{ in, want string }{
		{"45285.2", "452852"},
		{"0.00100000", "100000"},
		{"1.54571953", "154571953"},
		{"45283.5", "452835"},
		{"0.10000000", "10000000"},
		// Defensive only: a "0" qty is a delete and never reaches the
		// checksum path (see FmtVal doc comment).
		{"0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := FmtVal(tt.in); got != tt.want {
				t.Fatalf("FmtVal(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The official Kraken known-good vector (spec §6.5, ✅ reviewer-reproduced).
// Guards token formatting, ordering (asks-then-bids), and strip rules.
func TestBookChecksumKnownVector(t *testing.T) {
	asks := [][2]string{
		{"45285.2", "0.00100000"},
		{"45286.4", "1.54571953"},
		{"45286.6", "1.54571109"},
		{"45289.6", "1.54560911"},
		{"45290.2", "0.15890660"},
		{"45291.8", "1.54553491"},
		{"45294.7", "0.04454749"},
		{"45296.1", "0.35380000"},
		{"45297.5", "0.09945542"},
		{"45299.5", "0.18772827"},
	}
	bids := [][2]string{
		{"45283.5", "0.10000000"},
		{"45283.4", "1.54582015"},
		{"45282.1", "0.10000000"},
		{"45281.0", "0.10000000"},
		{"45280.3", "1.54592586"},
		{"45279.0", "0.07990000"},
		{"45277.6", "0.03310103"},
		{"45277.5", "0.30000000"},
		{"45277.3", "1.54602737"},
		{"45276.6", "0.15445238"},
	}
	const want = uint32(3310070434)
	if got := BookChecksum(asks, bids); got != want {
		t.Fatalf("BookChecksum = %d, want %d", got, want)
	}
}

// Order matters: swapping asks/bids must change the checksum.
func TestBookChecksumOrderSensitive(t *testing.T) {
	asks := [][2]string{{"45285.2", "0.00100000"}}
	bids := [][2]string{{"45283.5", "0.10000000"}}
	if BookChecksum(asks, bids) == BookChecksum(bids, asks) {
		t.Fatal("checksum must depend on asks-then-bids ordering")
	}
}
