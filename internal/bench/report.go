package bench

import (
	"fmt"
	"io"
	"os"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
)

// reportPercentiles is the tail we always report — never just a mean ("the
// mean is meaningless" for latency), and always Max.
var reportPercentiles = []float64{50, 90, 99, 99.9, 99.99}

// Result bundles one metric's histogram with its honest labeling.
type Result struct {
	Name  string // "apply" | "read"
	Hist  *hdrhistogram.Histogram
	Basis string // measurement basis, printed verbatim
}

// errWriter accumulates the first write error so every Fprintf is checked.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err == nil {
		_, ew.err = fmt.Fprintf(ew.w, format, args...)
	}
}

// PrintReport writes the disclosure block, both percentile tables, and the
// HdrHistogram plotter dumps (the format the free online plotter consumes).
// It REFUSES to print anything if the disclosure is invalid.
func PrintReport(w io.Writer, d Disclosure, cfg ApplyConfig, results []Result) error {
	if err := d.Validate(); err != nil {
		return err
	}

	ew := &errWriter{w: w}
	ew.printf("── hardware / environment disclosure ─────────────────────────\n")
	ew.printf("  CPU:        %s (%d cores)\n", d.CPUModel, d.Cores)
	ew.printf("  RAM:        %s\n", d.RAM)
	ew.printf("  OS:         %s\n", d.OS)
	ew.printf("  Go:         %s   GOMAXPROCS=%d\n", d.GoVersion, d.GOMAXPROCS)
	ew.printf("  -race:      off (enforced)\n")
	ew.printf("  idle:       %v (operator-attested)\n", d.Idle)
	ew.printf("  replay:     %d events/s target, %d samples (+%d warm-up, discarded)\n",
		cfg.Rate, cfg.Samples, cfg.Warmup)
	ew.printf("  CO mode:    %s\n\n", cfg.Mode)

	for _, r := range results {
		ew.printf("── %s ──\n", metricTitle(r.Name))
		ew.printf("  basis: %s\n", r.Basis)
		ew.printf("  %10s  %14s\n", "percentile", "latency")
		for _, p := range reportPercentiles {
			// DOC-CHECKED (pinned hdrhistogram-go v1.2.0): the accessor is
			// ValueAtPercentile and takes 0..100 (99.0 == p99).
			ew.printf("  %9.2f%%  %14s\n", p, time.Duration(r.Hist.ValueAtPercentile(p)))
		}
		ew.printf("  %10s  %14s\n", "max", time.Duration(r.Hist.Max()))
		ew.printf("  samples: %d\n\n", r.Hist.TotalCount())
	}
	if ew.err != nil {
		return ew.err
	}

	// Plotter-format dumps (paste into the free HdrHistogram online plotter).
	for _, r := range results {
		ew.printf("── HdrHistogram plotter format: %s ──\n", r.Name)
		if ew.err != nil {
			return ew.err
		}
		if _, err := r.Hist.PercentilesPrint(w, 1, 1.0); err != nil {
			return fmt.Errorf("plotter dump (%s): %w", r.Name, err)
		}
		ew.printf("\n")
	}
	return ew.err
}

// WriteCSV writes `metric,percentile,latency_ns` rows for every metric —
// portable, plots anywhere.
func WriteCSV(path string, results []Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // error surfaced via the write/Sync path below
	ew := &errWriter{w: f}
	ew.printf("metric,percentile,latency_ns\n")
	for _, r := range results {
		for _, p := range reportPercentiles {
			ew.printf("%s,%.2f,%d\n", r.Name, p, r.Hist.ValueAtPercentile(p))
		}
		ew.printf("%s,100.00,%d\n", r.Name, r.Hist.Max())
	}
	if ew.err != nil {
		return ew.err
	}
	return f.Sync()
}

func metricTitle(name string) string {
	switch name {
	case "apply":
		return "HEADLINE — end-to-end APPLY latency (event-received → snapshot-published)"
	case "read":
		return "footnote — READ path (wait-free atomic Load + field reads)"
	default:
		return name
	}
}
