package model

import "testing"

func TestPriceFromString(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64 // scaled by 1e8
		wantErr bool
	}{
		{name: "two decimals", in: "10101.10", want: 1_010_110_000_000},
		{name: "small qty style", in: "0.00100000", want: 100_000},
		{name: "one decimal", in: "45285.2", want: 4_528_520_000_000},
		{name: "zero", in: "0", want: 0},
		{name: "integer", in: "42", want: 4_200_000_000},
		{name: "full 8 decimals", in: "1.54571953", want: 154_571_953},
		{name: "trailing zeros beyond scale ok", in: "1.230000000", want: 123_000_000},
		{name: "empty", in: "", wantErr: true},
		{name: "letters", in: "abc", wantErr: true},
		{name: "two dots", in: "1.2.3", wantErr: true},
		{name: "lone dot", in: ".", wantErr: true},
		{name: "nonzero past scale", in: "1.000000001", wantErr: true},
		{name: "negative rejected", in: "-1.5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PriceFromString(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("PriceFromString(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PriceFromString(%q) error: %v", tt.in, err)
			}
			if int64(got) != tt.want {
				t.Fatalf("PriceFromString(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestQtyFromString(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "kraken qty", in: "4831.75496356", want: 483_175_496_356},
		{name: "delete qty", in: "0", want: 0},
		{name: "coinbase size", in: "0.45054140", want: 45_054_140},
		{name: "invalid", in: "1,5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := QtyFromString(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("QtyFromString(%q) = %d, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("QtyFromString(%q) error: %v", tt.in, err)
			}
			if int64(got) != tt.want {
				t.Fatalf("QtyFromString(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// String round-trip: parse(s).String() re-parses to the same scaled value.
func TestFixedPointStringRoundTrip(t *testing.T) {
	for _, s := range []string{"10101.10", "0.00100000", "45285.2", "0", "1.54571953", "99999999.99999999"} {
		t.Run(s, func(t *testing.T) {
			p, err := PriceFromString(s)
			if err != nil {
				t.Fatalf("parse %q: %v", s, err)
			}
			back, err := PriceFromString(p.String())
			if err != nil {
				t.Fatalf("re-parse %q: %v", p.String(), err)
			}
			if back != p {
				t.Fatalf("round-trip %q -> %q: %d != %d", s, p.String(), back, p)
			}
		})
	}
}

func TestEnumStrings(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{Bid.String(), "bid"},
		{Ask.String(), "ask"},
		{Coinbase.String(), "coinbase"},
		{Kraken.String(), "kraken"},
		{KindSnapshot.String(), "snapshot"},
		{KindUpdate.String(), "update"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("String() = %q, want %q", tt.got, tt.want)
		}
	}
}
