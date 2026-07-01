// Command tickstream-bench is the honest latency harness (spec §9).
//
// Headline: end-to-end APPLY latency, driven open-loop from a replayed feed
// with intended-time measurement (coordinated-omission-safe — this path can
// stall, so CO correction is load-bearing). Footnote: the wait-free read
// path (~tens of ns; an atomic Load cannot stall, so CO correction there is
// demonstrative-only). Refuses to emit results without a complete hardware
// disclosure or when built with -race.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nyaungnicholas-wq/tickstream/internal/bench"
)

func main() {
	var (
		rate        = flag.Int("rate", 5_000, "apply replay target rate (events/second)")
		samples     = flag.Int("samples", 200_000, "recorded apply samples (after warm-up)")
		warmup      = flag.Int("warmup", 20_000, "warm-up events (discarded)")
		readSamples = flag.Int("read-samples", 1_000_000, "read-path samples")
		mode        = flag.String("mode", "intended", "CO handling on the apply path: intended (open-loop) | corrected (RecordCorrectedValue backfill)")
		csvPath     = flag.String("csv", "latency.csv", "CSV output path")
		idle        = flag.Bool("idle", true, "attest that the machine is otherwise idle (recorded in the disclosure)")
	)
	flag.Parse()

	// Disclosure first: refuse to do any work that could end in a
	// placeholder result (also rejects -race builds).
	disc, err := bench.Collect(*idle)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg := bench.ApplyConfig{Rate: *rate, Samples: *samples, Warmup: *warmup, Mode: bench.Mode(*mode)}

	fmt.Printf("running apply benchmark: %d events at %d/s (%s mode)…\n", *samples, *rate, *mode)
	applyHist, err := bench.RunApply(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("running read benchmark: %d samples…\n\n", *readSamples)
	readHist, err := bench.RunRead(*readSamples)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	results := []bench.Result{
		{
			Name: "apply",
			Hist: applyHist,
			Basis: "open-loop replay, intended-time (CO-SAFE: this path can stall, " +
				"so coordinated-omission handling is load-bearing)",
		},
		{
			Name: "read",
			Hist: readHist,
			Basis: "wait-free atomic Load; CO correction demonstrative-only (a wait-free " +
				"load cannot stall); includes ~2x time.Now() timer overhead per sample",
		},
	}
	if cfg.Mode == bench.ModeCorrected {
		results[0].Basis = "closed-loop + RecordCorrectedValue backfill (CO-corrected)"
	}

	if err := bench.PrintReport(os.Stdout, disc, cfg, results); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := bench.WriteCSV(*csvPath, results); err != nil {
		fmt.Fprintln(os.Stderr, "write csv:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *csvPath)
}
