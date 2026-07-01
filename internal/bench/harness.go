// Package bench is the latency harness (spec §9).
//
// THE HEADLINE METRIC IS END-TO-END APPLY LATENCY — event-received →
// snapshot-published — measured OPEN-LOOP from each event's INTENDED
// (scheduled) arrival time, the wrk2 approach. This path can queue and stall
// (single writer, bounded hand-off), so coordinated-omission correction is
// LOAD-BEARING here: an engine stall shows up across every event it delayed,
// not just the one that happened to be measured.
//
// The read path is a WAIT-FREE FOOTNOTE: one atomic pointer Load plus field
// reads, tens of nanoseconds. An atomic Load never blocks, so there is no
// stall for CO machinery to correct on that path — we say so out loud rather
// than dressing the read number in methodology it doesn't need.
//
// Why not testing.B: it reports one aggregate ns/op and cannot produce a
// latency DISTRIBUTION; the tail (p99/p99.9/max) is the entire point.
package bench

import (
	"fmt"
	"runtime"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/nyaungnicholas-wq/tickstream/internal/engine"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
	"github.com/nyaungnicholas-wq/tickstream/internal/snapshot"
)

// Mode selects how coordinated omission is handled on the apply path.
type Mode string

const (
	// ModeIntended (default): open-loop pacing, latency measured from each
	// event's scheduled arrival time (CO-safe by construction; wrk2-style).
	ModeIntended Mode = "intended"
	// ModeCorrected: closed-loop measurement backfilled with the library's
	// RecordCorrectedValue(v, expectedInterval). DOC-CHECKED: that is the Go
	// method name in the pinned hdrhistogram-go (Java's is
	// recordValueWithExpectedInterval).
	ModeCorrected Mode = "corrected"
)

// ApplyConfig configures the headline benchmark.
type ApplyConfig struct {
	Rate    int  // target events/second
	Samples int  // recorded events (after warm-up)
	Warmup  int  // discarded warm-up events
	Mode    Mode // intended | corrected
}

// histogram bounds: 1ns .. 60s at 3 significant figures (~0.1% precision).
func newHist() *hdrhistogram.Histogram {
	return hdrhistogram.New(1, 60_000_000_000, 3)
}

// RunApply drives the REAL engine (book apply + truncate + crossed guard +
// CRC32 verify + fresh-snapshot publish) with a deterministic replayed feed
// at cfg.Rate. Decode is excluded by design: the metric starts where a frame
// has just been decoded (spec §9.1).
func RunApply(cfg ApplyConfig) (*hdrhistogram.Histogram, error) {
	if cfg.Rate <= 0 || cfg.Samples <= 0 {
		return nil, fmt.Errorf("bench: rate and samples must be positive")
	}
	total := cfg.Warmup + cfg.Samples
	events := generateEvents(total)

	pub := &snapshot.Publisher{}
	resyncs := 0
	eng := engine.New(pub, []model.Venue{model.Kraken},
		map[model.Venue]func(){model.Kraken: func() { resyncs++ }}, 10)

	hist := newHist()
	interval := time.Duration(int64(time.Second) / int64(cfg.Rate))

	// Warm-up (JIT/cache/GC settle), then Reset before recording.
	for i := 0; i < cfg.Warmup; i++ {
		events[i].Received = time.Now()
		eng.Apply(events[i])
	}
	hist.Reset()
	runtime.GC() // start recording from a settled heap

	dropped := 0
	start := time.Now()
	for i := cfg.Warmup; i < total; i++ {
		scheduled := start.Add(time.Duration(int64(i-cfg.Warmup)) * interval)
		waitUntil(scheduled)

		switch cfg.Mode {
		case ModeCorrected:
			// Closed-loop measure + library backfill of the samples a
			// stall would have swallowed.
			t0 := time.Now()
			events[i].Received = t0
			eng.Apply(events[i])
			if err := hist.RecordCorrectedValue(time.Since(t0).Nanoseconds(), interval.Nanoseconds()); err != nil {
				dropped++
			}
		default: // ModeIntended
			// Open-loop: the event is "due" at scheduled regardless of how
			// backed-up the engine is; latency runs from the INTENDED time,
			// so backlog shows up in every delayed event's number.
			events[i].Received = scheduled
			eng.Apply(events[i])
			if err := hist.RecordValue(time.Since(scheduled).Nanoseconds()); err != nil {
				dropped++ // out-of-range values are DROPPED — surface, never hide
			}
		}
	}
	if resyncs > 0 {
		return nil, fmt.Errorf("bench: %d resync(s) fired during replay — generated checksums are broken, results invalid", resyncs)
	}
	if dropped > 0 {
		return nil, fmt.Errorf("bench: %d sample(s) exceeded the histogram's trackable range — size it up", dropped)
	}
	return hist, nil
}

// readSink defeats dead-code elimination of the read loop.
var readSink int64

// RunRead measures the wait-free read path: one atomic Load plus reads of the
// consolidated top-of-book and signals.
//
// HONESTY NOTES baked into how this is reported:
//   - each sample carries the ~2×time.Now() timer overhead (tens of ns —
//     comparable to the operation!), so treat the numbers as an upper bound;
//   - an atomic.Load can NEVER block, so coordinated-omission correction is
//     demonstrative-only here — there is no stall to correct. This number is
//     the footnote, not the headline. Do not call it "sub-millisecond".
func RunRead(samples int) (*hdrhistogram.Histogram, error) {
	// Publish one realistic snapshot for readers to load.
	pub := &snapshot.Publisher{}
	eng := engine.New(pub, []model.Venue{model.Kraken}, nil, 10)
	seed := generateEvents(1)
	seed[0].Received = time.Now()
	eng.Apply(seed[0])
	if pub.Load() == nil {
		return nil, fmt.Errorf("bench: no snapshot published")
	}

	hist := newHist()
	// Warm-up.
	for i := 0; i < samples/10; i++ {
		s := pub.Load()
		readSink += s.ApplyLatencyNanos + int64(s.Consolidated.BestBidPrice)
	}
	hist.Reset()

	for i := 0; i < samples; i++ {
		t0 := time.Now()
		s := pub.Load() // the wait-free read
		readSink += int64(s.Consolidated.BestBidPrice) + int64(s.Consolidated.BestAskPrice) +
			int64(s.SpreadTicks) + int64(s.ImbalanceSigned*1e6)
		if err := hist.RecordValue(time.Since(t0).Nanoseconds()); err != nil {
			return nil, fmt.Errorf("bench: read sample out of range: %w", err)
		}
	}
	if readSink == 0 {
		// Unreachable with real data; exists to keep the sink live so the
		// compiler cannot eliminate the field reads being measured.
		return nil, fmt.Errorf("bench: read sink empty")
	}
	return hist, nil
}

// waitUntil sleeps coarsely, then spins the final stretch — time.Sleep alone
// overshoots by ~ms on macOS, and an oversleep would be misattributed to the
// engine under intended-time measurement.
func waitUntil(t time.Time) {
	for {
		d := time.Until(t)
		if d <= 0 {
			return
		}
		if d > time.Millisecond {
			time.Sleep(d - time.Millisecond)
		} else {
			runtime.Gosched()
		}
	}
}
