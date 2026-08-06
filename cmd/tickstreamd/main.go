// Command tickstreamd runs the TickStream consolidation daemon: one goroutine
// per venue feed, one bounded hand-off channel, ONE single-writer engine, and
// a wait-free snapshot read path. main.go does flag parsing, wiring, and
// signal handling only — all logic lives in internal/.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/capture"
	"github.com/nyaungnicholas-wq/tickstream/internal/engine"
	"github.com/nyaungnicholas-wq/tickstream/internal/feed"
	"github.com/nyaungnicholas-wq/tickstream/internal/feed/coinbase"
	"github.com/nyaungnicholas-wq/tickstream/internal/feed/kraken"
	"github.com/nyaungnicholas-wq/tickstream/internal/httpapi"
	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
	"github.com/nyaungnicholas-wq/tickstream/internal/snapshot"
)

const version = "0.5.0"

// engineChanCap bounds the feed→engine hand-off (spec §4.4). A full buffer is
// a DEFINED failure mode — the feeds drop + count + resync, never block their
// read loops (see internal/feed/*/feed.go).
const engineChanCap = 4096

func main() {
	var (
		venuesFlag  = flag.String("venues", "coinbase,kraken", "comma-separated venues to run (coinbase,kraken)")
		cbSymbol    = flag.String("coinbase-symbol", "BTC-USD", "Coinbase Exchange product id")
		krSymbol    = flag.String("kraken-symbol", "BTC/USD", "Kraken v2 symbol")
		depth       = flag.Int("depth", 10, "Kraken book depth (10|25|100|500|1000)")
		httpAddr    = flag.String("http", "127.0.0.1:8321", "dashboard listen address (empty disables)")
		captureDir  = flag.String("capture-dir", "", "record the raw event tape to this directory (empty disables)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	fmt.Printf("tickstream v%s\n", version)
	if *showVersion {
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var feeds []feed.Feed
	for _, v := range strings.Split(*venuesFlag, ",") {
		switch strings.TrimSpace(v) {
		case "coinbase":
			feeds = append(feeds, coinbase.New(*cbSymbol))
		case "kraken":
			feeds = append(feeds, kraken.New(*krSymbol, *depth))
		case "":
		default:
			fmt.Fprintf(os.Stderr, "unknown venue %q\n", v)
			os.Exit(2)
		}
	}
	if len(feeds) == 0 {
		fmt.Fprintln(os.Stderr, "no venues configured")
		os.Exit(2)
	}

	venues := make([]model.Venue, 0, len(feeds))
	resyncFns := make(map[model.Venue]func(), len(feeds))
	for _, f := range feeds {
		venues = append(venues, f.Venue())
		resyncFns[f.Venue()] = f.RequestResync
	}

	// Optional tape recorder. The execution-cost study replays this, so the
	// depth the tape was recorded at is the hard ceiling on the order size it
	// can ever answer for — a depth-10 BTC book is a couple of BTC. Refuse to
	// start a long capture at the default depth rather than discover two weeks
	// later that the tape is too thin to measure anything.
	var symbols = map[string]string{}
	for _, f := range feeds {
		switch f.Venue() {
		case model.Coinbase:
			symbols[model.Coinbase.String()] = *cbSymbol
		case model.Kraken:
			symbols[model.Kraken.String()] = *krSymbol
		}
	}
	var rec *capture.Writer
	var engOpts []engine.Option
	if *captureDir != "" {
		if *depth < 100 {
			fmt.Fprintf(os.Stderr, "refusing to capture at -depth %d: the tape caps the order size the study can price; use -depth 500 or 1000\n", *depth)
			os.Exit(2)
		}
		venueNames := make([]string, 0, len(venues))
		for _, v := range venues {
			venueNames = append(venueNames, v.String())
		}
		w, err := capture.NewWriter(capture.Config{
			Dir: *captureDir, Venues: venueNames, Symbols: symbols, Depth: *depth,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "capture: %v\n", err)
			os.Exit(1)
		}
		rec = w
		engOpts = append(engOpts, engine.WithSink(w.Offer))
		fmt.Printf("capturing to %s (depth %d)\n", *captureDir, *depth)
	}

	pub := &snapshot.Publisher{}
	eng := engine.New(pub, venues, resyncFns, *depth, engOpts...)
	events := make(chan model.Event, engineChanCap)

	var wg sync.WaitGroup

	// Goroutine-per-feed ingestion (§4.1 rule 1).
	for _, f := range feeds {
		wg.Add(1)
		go func(f feed.Feed) {
			defer wg.Done()
			if err := f.Run(ctx, events); err != nil && ctx.Err() == nil {
				slog.Error("feed exited", "venue", f.Venue(), "err", err)
			}
		}(f)
	}

	// THE single writer (§4.1 rule 2).
	wg.Add(1)
	go func() {
		defer wg.Done()
		eng.Run(ctx, events)
	}()

	// A reader: the local dashboard (HTML + JSON snapshot endpoint).
	if *httpAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := httpapi.Serve(ctx, *httpAddr, pub); err != nil {
				slog.Error("dashboard server exited", "err", err)
			}
		}()
	}

	// A reader: 1s status line off the wait-free snapshot load.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				printStatus(pub.Load())
			}
		}
	}()

	<-ctx.Done()
	fmt.Println("\nshutting down…")
	wg.Wait()
	// Close the recorder only after the single writer has stopped calling
	// Offer, so the tail of the tape is not lost to a race with shutdown.
	if rec != nil {
		st := rec.Stats()
		if err := rec.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "capture close: %v\n", err)
		}
		fmt.Printf("capture: written=%d dropped=%d files=%d\n", st.Written, st.Dropped, st.Files)
	}
	fmt.Printf("done. drops=%d resyncs=%d checksum_mismatches=%d thin_skips=%d reconnects=%d\n",
		metrics.DroppedEvents.Load(), metrics.Resyncs.Load(),
		metrics.ChecksumMismatches.Load(), metrics.ChecksumSkippedThin.Load(),
		metrics.Reconnects.Load())
}

// printStatus renders the consolidated NBBO view (M4). Reads are one atomic
// Load plus field reads — the wait-free path.
func printStatus(s *model.Snapshot) {
	if s == nil {
		fmt.Println("waiting for first snapshot…")
		return
	}
	c := s.Consolidated
	if !c.Valid {
		fmt.Println("no quoting venue yet (books resyncing)…")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "NBBO bid=%s@%s ask=%s@%s mid=%.2f wmid=%.2f imb=%+.3f spread=%s",
		c.BestBidPrice, venueTag(c.BestBidVenue),
		c.BestAskPrice, venueTag(c.BestAskVenue),
		s.Mid, s.WeightedMid, s.ImbalanceSigned, c.Spread)
	if s.CrossVenueCrossed {
		b.WriteString("  [XVENUE-CROSSED]")
	}
	fmt.Fprintf(&b, "  applyLat=%s drops=%d resyncs=%d",
		time.Duration(s.ApplyLatencyNanos), metrics.DroppedEvents.Load(), metrics.Resyncs.Load())
	fmt.Println(b.String())
}

func venueTag(v model.Venue) string {
	if v == model.Kraken {
		return "KR"
	}
	return "CB"
}
