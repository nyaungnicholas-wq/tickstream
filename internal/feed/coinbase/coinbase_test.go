package coinbase

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "coinbase", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeSnapshot(t *testing.T) {
	ev, ok, err := DecodeMessage(fixture(t, "snapshot.json"))
	if err != nil || !ok {
		t.Fatalf("DecodeMessage: ok=%v err=%v", ok, err)
	}
	if ev.Venue != model.Coinbase || ev.Kind != model.KindSnapshot || ev.Symbol != "BTC-USD" {
		t.Fatalf("event header wrong: %+v", ev)
	}
	if len(ev.Bids) != 2 || len(ev.Asks) != 2 {
		t.Fatalf("levels = %d bids / %d asks, want 2/2", len(ev.Bids), len(ev.Asks))
	}
	wantPrice, _ := model.PriceFromString("10101.10")
	wantQty, _ := model.QtyFromString("0.45054140")
	if ev.Bids[0].Price != wantPrice || ev.Bids[0].Qty != wantQty {
		t.Fatalf("bid[0] = %+v", ev.Bids[0])
	}
	if ev.Checksum != 0 {
		t.Fatalf("coinbase events carry no checksum, got %d", ev.Checksum)
	}
}

func TestDecodeL2Update(t *testing.T) {
	ev, ok, err := DecodeMessage(fixture(t, "l2update.json"))
	if err != nil || !ok {
		t.Fatalf("DecodeMessage: ok=%v err=%v", ok, err)
	}
	if ev.Kind != model.KindUpdate {
		t.Fatalf("kind = %v, want update", ev.Kind)
	}
	// Side mapping: "buy" -> Bids, "sell" -> Asks.
	if len(ev.Bids) != 1 || len(ev.Asks) != 1 {
		t.Fatalf("levels = %d bids / %d asks, want 1/1", len(ev.Bids), len(ev.Asks))
	}
	// Absolute size (not delta) is carried through as-is.
	wantQty, _ := model.QtyFromString("0.162567")
	if ev.Bids[0].Qty != wantQty {
		t.Fatalf("bid qty = %d, want %d (absolute size)", ev.Bids[0].Qty, wantQty)
	}
	// size "0" -> Qty 0 (a delete level).
	if ev.Asks[0].Qty != 0 {
		t.Fatalf("ask qty = %d, want 0 (delete)", ev.Asks[0].Qty)
	}
	if ev.EventTimeNanos == 0 {
		t.Fatal("EventTimeNanos not parsed from the time field")
	}
}

func TestIgnoredFrames(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"subscriptions ack", `{"type":"subscriptions","channels":[]}`},
		{"heartbeat", `{"type":"heartbeat","sequence":90,"product_id":"BTC-USD"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := DecodeMessage([]byte(tt.in))
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if ok {
				t.Fatal("ok = true for a non-book frame")
			}
		})
	}
}

func TestUnknownSideIsError(t *testing.T) {
	in := `{"type":"l2update","product_id":"BTC-USD","changes":[["hold","1.0","2.0"]]}`
	if _, _, err := DecodeMessage([]byte(in)); err == nil {
		t.Fatal("unknown side must be an error")
	}
}
