package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// fakeClock is mutex-guarded because the drain goroutine reads it concurrently
// with the test goroutine advancing it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock        { return &fakeClock{t: t} }
func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func mustPrice(t *testing.T, s string) model.Price {
	t.Helper()
	p, err := model.PriceFromString(s)
	if err != nil {
		t.Fatalf("PriceFromString(%q): %v", s, err)
	}
	return p
}

func mustQty(t *testing.T, s string) model.Qty {
	t.Helper()
	q, err := model.QtyFromString(s)
	if err != nil {
		t.Fatalf("QtyFromString(%q): %v", s, err)
	}
	return q
}

// makeEvents builds a deterministic mix of snapshots, updates and trades.
func makeEvents(t *testing.T, n int) []model.Event {
	t.Helper()
	base := time.Date(2026, 8, 6, 3, 30, 0, 0, time.UTC)
	out := make([]model.Event, 0, n)
	for i := 0; i < n; i++ {
		venue := model.Coinbase
		if i%2 == 1 {
			venue = model.Kraken
		}
		ev := model.Event{
			Venue:          venue,
			Symbol:         "BTC-USD",
			EventTimeNanos: base.Add(time.Duration(i) * time.Millisecond).UnixNano(),
			Received:       base.Add(time.Duration(i) * time.Millisecond),
			Checksum:       uint32(i * 7),
		}
		switch i % 3 {
		case 0:
			ev.Kind = model.KindSnapshot
			for j := 0; j < 10; j++ {
				ps := fmt.Sprintf("64%03d.%02d", i%1000, j)
				qs := fmt.Sprintf("0.%08d", j+1)
				ev.Bids = append(ev.Bids, model.Level{
					Price: mustPrice(t, ps), Qty: mustQty(t, qs), PriceStr: ps, QtyStr: qs,
				})
				ev.Asks = append(ev.Asks, model.Level{
					Price: mustPrice(t, ps), Qty: mustQty(t, qs), PriceStr: ps, QtyStr: qs,
				})
			}
		case 1:
			ev.Kind = model.KindUpdate
			ps := fmt.Sprintf("648%02d.5", i%100)
			qs := "1.25000000"
			ev.Bids = append(ev.Bids, model.Level{
				Price: mustPrice(t, ps), Qty: mustQty(t, qs), PriceStr: ps, QtyStr: qs,
			})
		default:
			ev.Kind = model.KindTrade
			for j := 0; j <= i%3; j++ {
				ps := fmt.Sprintf("6481%d.25", j)
				qs := fmt.Sprintf("0.0%d000000", j+1)
				side := model.Bid
				if j%2 == 1 {
					side = model.Ask
				}
				ev.Trades = append(ev.Trades, model.Trade{
					Price: mustPrice(t, ps), Qty: mustQty(t, qs),
					PriceStr: ps, QtyStr: qs, Aggressor: side,
					TimeNanos: base.Add(time.Duration(i) * time.Millisecond).UnixNano(),
					ID:        fmt.Sprintf("t-%d-%d", i, j),
				})
			}
		}
		out = append(out, ev)
	}
	return out
}

func writeAll(t *testing.T, cfg Config, evs []model.Event) *Writer {
	t.Helper()
	w, err := NewWriter(cfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, ev := range evs {
		w.Offer(ev)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return w
}

func onlyRun(t *testing.T, dir string) RunInfo {
	t.Helper()
	runs, err := Runs(dir)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("want exactly 1 run, got %d", len(runs))
	}
	return runs[0]
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := makeEvents(t, 200)
	writeAll(t, Config{Dir: dir, Venues: []string{"coinbase", "kraken"},
		Symbols: map[string]string{"coinbase": "BTC-USD"}, Depth: 1000}, want)

	run := onlyRun(t, dir)
	var got []model.Event
	rep, err := ReplayRun(dir, run.RunID, func(r Record) error {
		if r.Gap == 0 {
			got = append(got, r.Event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	if rep.Gaps != 0 || rep.GapEvents != 0 {
		t.Fatalf("unexpected loss: gaps=%d gapEvents=%d", rep.Gaps, rep.GapEvents)
	}
	if len(got) != len(want) {
		t.Fatalf("record count: got %d want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Venue != g.Venue || w.Kind != g.Kind || w.Symbol != g.Symbol {
			t.Fatalf("event %d header: got (%v,%v,%q) want (%v,%v,%q)",
				i, g.Venue, g.Kind, g.Symbol, w.Venue, w.Kind, w.Symbol)
		}
		if w.Checksum != g.Checksum || w.EventTimeNanos != g.EventTimeNanos {
			t.Fatalf("event %d checksum/time: got (%d,%d) want (%d,%d)",
				i, g.Checksum, g.EventTimeNanos, w.Checksum, w.EventTimeNanos)
		}
		// gob drops the monotonic reading; compare wall clock only.
		if !w.Received.Equal(g.Received) {
			t.Fatalf("event %d Received: got %v want %v", i, g.Received, w.Received)
		}
		checkLevels(t, i, "bids", w.Bids, g.Bids)
		checkLevels(t, i, "asks", w.Asks, g.Asks)
		if len(w.Trades) != len(g.Trades) {
			t.Fatalf("event %d trades: got %d want %d", i, len(g.Trades), len(w.Trades))
		}
		for j := range w.Trades {
			wt, gt := w.Trades[j], g.Trades[j]
			if wt != gt {
				t.Fatalf("event %d trade %d: got %+v want %+v", i, j, gt, wt)
			}
		}
	}
}

func checkLevels(t *testing.T, i int, side string, want, got []model.Level) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("event %d %s: got %d levels want %d", i, side, len(got), len(want))
	}
	for j := range want {
		if want[j] != got[j] {
			t.Fatalf("event %d %s[%d]: got %+v want %+v", i, side, j, got[j], want[j])
		}
	}
}

// TestRotationByHour is the regression test for the rotation deadlock: the
// rotation path used to re-enter a non-reentrant mutex, so the first hour
// boundary hung the drain goroutine forever and the tape silently stopped.
func TestRotationByHour(t *testing.T) {
	dir := t.TempDir()
	clk := newClock(time.Date(2026, 8, 6, 3, 59, 0, 0, time.UTC))
	evs := makeEvents(t, 40)

	w, err := NewWriter(Config{Dir: dir, Depth: 10, Now: clk.Now})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, ev := range evs[:20] {
		w.Offer(ev)
	}
	clk.Advance(2 * time.Hour) // cross a boundary
	for _, ev := range evs[20:] {
		w.Offer(ev)
	}

	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not return: rotation deadlocked")
	}

	run := onlyRun(t, dir)
	if run.Files < 2 {
		t.Fatalf("want >=2 files after crossing an hour boundary, got %d", run.Files)
	}
	assertLossless(t, dir, run.RunID, len(evs))
}

func TestRotationByMaxBytes(t *testing.T) {
	dir := t.TempDir()
	// Enough data that gzip actually emits blocks to the file: the byte
	// counter sees compressed output, so a handful of small records can sit
	// entirely inside gzip's buffer and never trip the threshold.
	evs := makeEvents(t, 6000)
	writeAll(t, Config{Dir: dir, Depth: 10, MaxBytes: 4096}, evs)

	run := onlyRun(t, dir)
	if run.Files < 2 {
		t.Fatalf("want >=2 files with MaxBytes=4096, got %d", run.Files)
	}
	assertLossless(t, dir, run.RunID, len(evs))
}

func assertLossless(t *testing.T, dir, runID string, want int) {
	t.Helper()
	n := 0
	rep, err := ReplayRun(dir, runID, func(r Record) error {
		if r.Gap == 0 {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	if rep.Gaps != 0 {
		t.Fatalf("unexpected gaps: %d (%d events)", rep.Gaps, rep.GapEvents)
	}
	if n != want {
		t.Fatalf("replayed %d records, want %d", n, want)
	}
}

func TestGapRecorded(t *testing.T) {
	dir := t.TempDir()
	evs := makeEvents(t, 5000)

	w, err := NewWriter(Config{Dir: dir, Depth: 10, BufSize: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, ev := range evs {
		w.Offer(ev)
	}
	st := w.Stats()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st.Dropped == 0 {
		t.Skip("no drops occurred; drain kept up with a BufSize of 1")
	}

	run := onlyRun(t, dir)
	sawGap := false
	rep, err := ReplayRun(dir, run.RunID, func(r Record) error {
		if r.Gap > 0 {
			sawGap = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	if !sawGap || rep.Gaps == 0 || rep.GapEvents == 0 {
		t.Fatalf("drops happened (%d) but the tape reports no gap: gaps=%d gapEvents=%d",
			st.Dropped, rep.Gaps, rep.GapEvents)
	}
}

func TestOfferNeverBlocksAndIsSafeAfterClose(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(Config{Dir: dir, Depth: 10, BufSize: 1})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ev := makeEvents(t, 1)[0]

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			w.Offer(ev)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Offer blocked: it must drop rather than stall the single writer")
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Must not panic or block once closed.
	w.Offer(ev)
	_ = w.Stats()
}

func TestTruncatedTailTolerated(t *testing.T) {
	dir := t.TempDir()
	evs := makeEvents(t, 120)
	writeAll(t, Config{Dir: dir, Depth: 10}, evs)

	run := onlyRun(t, dir)
	files, _ := filepath.Glob(filepath.Join(dir, "cap-*.gob.gz"))
	last := files[len(files)-1]
	fi, err := os.Stat(last)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(last, fi.Size()-40); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	rep, err := ReplayRun(dir, run.RunID, func(Record) error { return nil })
	if err != nil {
		t.Fatalf("a truncated FINAL file is the normal result of a crash and must not error: %v", err)
	}
	if !rep.Truncated {
		t.Fatal("Truncated flag not set on a truncated tape")
	}
}

func TestMissingSeqIsError(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, Config{Dir: dir, Depth: 10, MaxBytes: 4096}, makeEvents(t, 6000))

	run := onlyRun(t, dir)
	files, _ := filepath.Glob(filepath.Join(dir, "cap-*.gob.gz"))
	if len(files) < 3 {
		t.Fatalf("need >=3 files to delete a middle one, got %d", len(files))
	}
	if err := os.Remove(files[len(files)/2]); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := ReplayRun(dir, run.RunID, func(Record) error { return nil }); err == nil {
		t.Fatal("a tape with a missing file must not replay silently")
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	venues := []string{"coinbase", "kraken"}
	syms := map[string]string{"coinbase": "BTC-USD", "kraken": "BTC/USD"}
	writeAll(t, Config{Dir: dir, Venues: venues, Symbols: syms, Depth: 500}, makeEvents(t, 10))

	run := onlyRun(t, dir)
	if run.Header.Format != FormatVersion {
		t.Fatalf("Format: got %d want %d", run.Header.Format, FormatVersion)
	}
	if run.Header.Depth != 500 {
		t.Fatalf("Depth: got %d want 500", run.Header.Depth)
	}
	if run.RunID == "" {
		t.Fatal("empty RunID")
	}
	if len(run.Header.Venues) != 2 {
		t.Fatalf("Venues: got %v", run.Header.Venues)
	}
	if run.Header.Symbols["kraken"] != "BTC/USD" {
		t.Fatalf("Symbols: got %v", run.Header.Symbols)
	}
}

func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(Config{Dir: dir, Depth: 10})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Offer(makeEvents(t, 1)[0])
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got: %v", err)
	}
	_ = w.Stats()
}

// TestRunIDWithHyphensParses guards the filename parser: RunIDs are
// date-time-random and full of hyphens, so a parser that splits on the FIRST
// hyphen finds no runs at all.
func TestRunIDWithHyphensParses(t *testing.T) {
	dir := t.TempDir()
	const id = "20260806-014222-199089f5"
	writeAll(t, Config{Dir: dir, RunID: id, Depth: 10}, makeEvents(t, 5))

	run := onlyRun(t, dir)
	if run.RunID != id {
		t.Fatalf("RunID: got %q want %q", run.RunID, id)
	}
	assertLossless(t, dir, id, 5)
}
