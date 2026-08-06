// Command tapestat inspects a capture directory: it lists the runs on disk and
// replays each one, reporting what the tape actually contains.
//
// This exists because a capture is worthless until it has been read back. The
// counts it prints — records, gaps, lost events, truncation — are the integrity
// evidence for every measurement taken downstream: an execution-cost number
// computed over a tape with holes is a number computed against a book that
// skipped updates.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/capture"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

func main() {
	dir := flag.String("dir", "", "capture directory to inspect (required)")
	run := flag.String("run", "", "inspect only this run id (default: all)")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "tapestat: -dir is required")
		os.Exit(2)
	}

	runs, err := capture.Runs(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tapestat: %v\n", err)
		os.Exit(1)
	}
	if len(runs) == 0 {
		fmt.Fprintf(os.Stderr, "tapestat: no capture runs found in %s\n", *dir)
		os.Exit(1)
	}

	for _, r := range runs {
		if *run != "" && r.RunID != *run {
			continue
		}
		fmt.Printf("run %s  files=%d  started=%s  depth=%d  venues=%v\n",
			r.RunID, r.Files, r.Started.Format(time.RFC3339), r.Header.Depth, r.Header.Venues)

		var (
			byKind        = map[model.EventKind]uint64{}
			byVenue       = map[model.Venue]uint64{}
			tradesByVenue = map[model.Venue]uint64{}
			trades        uint64
			levels        uint64
			aggr          = map[model.Side]uint64{}
			first         time.Time
			last          time.Time
		)
		rep, err := capture.ReplayRun(*dir, r.RunID, func(rec capture.Record) error {
			if rec.Gap > 0 {
				return nil // counted by Replay itself
			}
			if first.IsZero() {
				first = rec.Wall
			}
			last = rec.Wall
			byKind[rec.Event.Kind]++
			byVenue[rec.Event.Venue]++
			levels += uint64(len(rec.Event.Bids) + len(rec.Event.Asks))
			for _, t := range rec.Event.Trades {
				trades++
				aggr[t.Aggressor]++
				tradesByVenue[rec.Event.Venue]++
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  REPLAY FAILED: %v\n", err)
			os.Exit(1)
		}

		span := last.Sub(first)
		fmt.Printf("  records=%d  gaps=%d  lost_events=%d  truncated=%v\n",
			rep.Records, rep.Gaps, rep.GapEvents, rep.Truncated)
		fmt.Printf("  span=%s", span.Round(time.Second))
		if span > 0 {
			fmt.Printf("  rate=%.1f ev/s", float64(rep.Records)/span.Seconds())
		}
		fmt.Println()
		for _, k := range []model.EventKind{model.KindSnapshot, model.KindUpdate, model.KindTrade} {
			fmt.Printf("  %-9s %d\n", k.String()+":", byKind[k])
		}
		for _, v := range []model.Venue{model.Coinbase, model.Kraken} {
			fmt.Printf("  %-9s %d\n", v.String()+":", byVenue[v])
		}
		fmt.Printf("  levels:   %d\n", levels)
		fmt.Printf("  trades:   %d (buyer-aggressed %d, seller-aggressed %d)\n",
			trades, aggr[model.Bid], aggr[model.Ask])

		// A tape with holes is still usable, but only if the holes are known.
		// Say so loudly rather than letting a later measurement inherit them.
		if rep.Gaps > 0 {
			fmt.Printf("  WARNING: %d gap(s) losing %d events — any cost measured across a gap is suspect\n",
				rep.Gaps, rep.GapEvents)
		}
		// A venue with book events but zero trades means its trade channel
		// silently failed to subscribe — the book path would look perfectly
		// healthy while half the tape's trade data was missing.
		for _, v := range []model.Venue{model.Coinbase, model.Kraken} {
			if byVenue[v] > 0 && tradesByVenue[v] == 0 {
				fmt.Printf("  WARNING: %s has %d book events but ZERO trades — trade channel likely not subscribed\n",
					v.String(), byVenue[v])
			}
		}
		fmt.Printf("  trades by venue: coinbase=%d kraken=%d\n",
			tradesByVenue[model.Coinbase], tradesByVenue[model.Kraken])
	}
}
