package kraken

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/checksum"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "kraken", name))
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
	if ev.Venue != model.Kraken || ev.Kind != model.KindSnapshot || ev.Symbol != "BTC/USD" {
		t.Fatalf("event header wrong: %+v", ev)
	}
	if ev.Checksum != 2439117997 {
		t.Fatalf("checksum = %d, want 2439117997 (must be carried through)", ev.Checksum)
	}
	// The EXACT wire strings must be preserved — the checksum depends on them.
	if ev.Bids[0].PriceStr != "0.5666" || ev.Bids[0].QtyStr != "4831.75496356" {
		t.Fatalf("wire strings not preserved: %+v", ev.Bids[0])
	}
	wantPrice, _ := model.PriceFromString("0.5666")
	if ev.Bids[0].Price != wantPrice {
		t.Fatalf("scaled price = %d, want %d", ev.Bids[0].Price, wantPrice)
	}
	if ev.EventTimeNanos == 0 {
		t.Fatal("EventTimeNanos not parsed")
	}
}

func TestDecodeUpdate(t *testing.T) {
	ev, ok, err := DecodeMessage(fixture(t, "update.json"))
	if err != nil || !ok {
		t.Fatalf("DecodeMessage: ok=%v err=%v", ok, err)
	}
	if ev.Kind != model.KindUpdate {
		t.Fatalf("kind = %v, want update", ev.Kind)
	}
	if len(ev.Bids) != 2 || len(ev.Asks) != 0 {
		t.Fatalf("levels = %d bids / %d asks, want 2/0 (empty side allowed)", len(ev.Bids), len(ev.Asks))
	}
	// qty 0 -> delete level.
	if ev.Bids[1].Qty != 0 {
		t.Fatalf("bids[1].Qty = %d, want 0 (delete)", ev.Bids[1].Qty)
	}
	if ev.Checksum != 2114181697 {
		t.Fatalf("checksum = %d, want 2114181697", ev.Checksum)
	}
}

func TestDecodeThinBook(t *testing.T) {
	ev, ok, err := DecodeMessage(fixture(t, "thin_book.json"))
	if err != nil || !ok {
		t.Fatalf("DecodeMessage: ok=%v err=%v", ok, err)
	}
	if len(ev.Bids) != 3 || len(ev.Asks) != 2 {
		t.Fatalf("thin book = %d bids / %d asks, want 3/2 (<10 per side)", len(ev.Bids), len(ev.Asks))
	}
}

func TestIgnoredFrames(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"subscribe ack", `{"method":"subscribe","result":{"channel":"book","symbol":"BTC/USD"},"success":true,"time_in":"t","time_out":"t"}`},
		{"heartbeat", `{"channel":"heartbeat"}`},
		{"status", `{"channel":"status","type":"update","data":[{"system":"online"}]}`},
		{"pong", `{"method":"pong","req_id":101}`},
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

// PART C integration: decode the known-good vector fixture, build the top-10
// [2]string slices from the PRESERVED wire strings, and assert BookChecksum
// reproduces the message's own checksum field (= 3310070434).
func TestSnapshotChecksumIntegration(t *testing.T) {
	ev, ok, err := DecodeMessage(fixture(t, "checksum_vector.json"))
	if err != nil || !ok {
		t.Fatalf("DecodeMessage: ok=%v err=%v", ok, err)
	}
	if len(ev.Asks) != 10 || len(ev.Bids) != 10 {
		t.Fatalf("vector = %d asks / %d bids, want 10/10", len(ev.Asks), len(ev.Bids))
	}
	asks := make([][2]string, 0, 10)
	for _, lv := range ev.Asks { // asks arrive low->high
		asks = append(asks, [2]string{lv.PriceStr, lv.QtyStr})
	}
	bids := make([][2]string, 0, 10)
	for _, lv := range ev.Bids { // bids arrive high->low
		bids = append(bids, [2]string{lv.PriceStr, lv.QtyStr})
	}
	got := checksum.BookChecksum(asks, bids)
	if got != ev.Checksum {
		t.Fatalf("BookChecksum = %d, want %d (fixture checksum field)", got, ev.Checksum)
	}
	if got != 3310070434 {
		t.Fatalf("BookChecksum = %d, want the known-good 3310070434", got)
	}
}
