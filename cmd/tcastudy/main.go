// Command tcastudy replays a recorded tape and measures what it would have
// cost to execute a market order, three ways.
//
// THE THREE ARMS
//
//	A naive  — the whole order, immediately, on ONE venue's book
//	B routed — the whole order, immediately, across BOTH venues' real depth
//	C sliced — the order split into N slices over a horizon, each one routed
//
// THE IMPACT BAND (why arm C is reported as a range, never a point)
//
// The one thing a recorded tape can never show is how the book would have
// reacted to YOUR order. Rather than invent an impact coefficient, arm C is
// bracketed by two bounds that are both pure measurement:
//
//	optimistic = arm C as recorded  — assumes the book fully replenishes
//	             between slices, so each slice pays the cost actually on tape
//	pessimistic = arm B             — assumes zero replenishment, so every
//	             slice competes for the same liquidity as one big order
//
// Reality is between them. Phase 5's resilience measurement (-resilience)
// says where: fast replenishment sits near the optimistic bound, slow near
// the pessimistic one. No free parameters anywhere.
//
// EVERY NUMBER IS A LOWER BOUND. See package execcost: displayed depth omits
// hidden and iceberg size, and makers pull quotes when they see aggression.
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/capture"
	"github.com/nyaungnicholas-wq/tickstream/internal/engine"
	"github.com/nyaungnicholas-wq/tickstream/internal/execcost"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
	"github.com/nyaungnicholas-wq/tickstream/internal/snapshot"
)

// ladderDepth is how many levels per side to pull from each venue's book.
// The tape is recorded at -depth 1000; taking all of it is what lets the
// study price sizes bigger than the touch.
const ladderDepth = 1000

func main() {
	var (
		dir      = flag.String("dir", "", "capture directory (required)")
		runID    = flag.String("run", "", "run id (default: the newest run)")
		sizesArg = flag.String("sizes", "0.1,0.5,1,2,5,10", "order sizes in BTC, comma separated")
		slices   = flag.Int("slices", 6, "number of TWAP slices for arm C")
		horizon  = flag.Duration("horizon", 5*time.Minute, "arm C execution horizon")
		every    = flag.Duration("every", 1*time.Minute, "how often to start a new decision")
		side     = flag.String("side", "buy", "buy (walks asks) or sell (walks bids)")
		outPath  = flag.String("out", "tca.csv", "CSV output path")
		volume   = flag.Float64("volume", 0, "30-day USD volume, for the fee tier")
		resil    = flag.Bool("resilience", true, "also measure book replenishment")
	)
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "tcastudy: -dir is required")
		os.Exit(2)
	}
	sizes, err := parseSizes(*sizesArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tcastudy: %v\n", err)
		os.Exit(2)
	}
	walkSide := model.Ask // buying takes offers
	if strings.EqualFold(*side, "sell") {
		walkSide = model.Bid
	}
	if *slices < 1 {
		fmt.Fprintln(os.Stderr, "tcastudy: -slices must be >= 1")
		os.Exit(2)
	}

	id, err := resolveRun(*dir, *runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tcastudy: %v\n", err)
		os.Exit(1)
	}

	fees := execcost.NewFeeTable(*volume, execcost.KrakenSpot, execcost.CoinbaseExchange)
	st := &study{
		fees:    fees,
		sizes:   sizes,
		side:    walkSide,
		slices:  *slices,
		horizon: *horizon,
		every:   *every,
		eng: engine.New(&snapshot.Publisher{},
			[]model.Venue{model.Coinbase, model.Kraken}, nil, ladderDepth),
	}
	if *resil {
		st.res = &resilience{}
	}

	rep, err := capture.ReplayRun(*dir, id, st.onRecord)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tcastudy: replay: %v\n", err)
		os.Exit(1)
	}
	st.finish()

	if err := writeCSV(*outPath, st.rows); err != nil {
		fmt.Fprintf(os.Stderr, "tcastudy: write csv: %v\n", err)
		os.Exit(1)
	}

	report(st, rep, id, *outPath, fees)
}

// ---------- decision bookkeeping ----------

// pending is one arm-C parent order still working through its slices.
type pending struct {
	rowIdx    int
	size      model.Qty
	remaining model.Qty
	nextAt    time.Time
	step      time.Duration
	left      int
	notional  execcost.Notional
	fee       execcost.Notional
	failed    bool

	// localWeighted accumulates each slice's slippage against the touch
	// prevailing AT THAT SLICE, size-weighted. Benchmarking the whole parent
	// against the arrival touch mixes execution cost with price drift over
	// the horizon, and over a few minutes drift dominates completely — the
	// pilot run showed arm C "beating" the touch purely because the market
	// happened to fall. This is the drift-free half.
	localWeighted float64
	qtyDone       model.Qty
}

// row is one decision: one size at one point in time, all three arms.
type row struct {
	At        time.Time
	SizeBTC   float64
	TouchPx   float64
	DepthBTC  float64
	NaiveBps  float64
	NaiveFee  float64
	RoutedBps float64
	RoutedFee float64
	RoutedKR  float64 // fraction of the order filled on Kraken
	// SlicedBps is implementation shortfall: cost vs the touch when the
	// decision was made. It includes horizon price drift.
	SlicedBps float64
	// SlicedLocalBps is pure execution cost: each slice vs the touch at its
	// own moment, size-weighted. Drift-free.
	SlicedLocalBps float64
	// DriftBps is what the market did to you while you worked the order —
	// the difference between the two, and NOT an execution result.
	DriftBps  float64
	SlicedFee float64
	NaiveErr  string
	RoutedErr string
	SlicedErr string
}

type study struct {
	fees    *execcost.FeeTable
	sizes   []model.Qty
	side    model.Side
	slices  int
	horizon time.Duration
	every   time.Duration
	eng     *engine.Engine
	res     *resilience

	now      time.Time
	nextDec  time.Time
	rows     []row
	pendings []*pending
	skipped  int
}

func (s *study) onRecord(r capture.Record) error {
	// A gap means the book we are about to price skipped updates. Refuse to
	// start a decision until a fresh interval begins after it.
	if r.Gap > 0 {
		s.pendings = nil // in-flight parents crossed a hole; drop them
		s.skipped++
		return nil
	}
	s.now = r.Wall
	s.eng.Apply(r.Event)

	if s.res != nil {
		s.res.observe(s.now, s.ladder(model.Ask))
	}
	s.advanceSlices()

	if s.nextDec.IsZero() {
		s.nextDec = s.now.Add(s.every) // let the books fill from the snapshot first
		return nil
	}
	if s.now.Before(s.nextDec) {
		return nil
	}
	s.nextDec = s.now.Add(s.every)
	s.startDecision()
	return nil
}

// ladder pulls the current consolidated one-side ladder out of the engine.
func (s *study) ladder(side model.Side) execcost.Ladder {
	per := map[model.Venue][]model.Level{
		model.Coinbase: s.eng.Levels(model.Coinbase, side, ladderDepth),
		model.Kraken:   s.eng.Levels(model.Kraken, side, ladderDepth),
	}
	return execcost.Merge(side, per)
}

func (s *study) startDecision() {
	l := s.ladder(s.side)
	if len(l) == 0 {
		return
	}
	for _, size := range s.sizes {
		r := row{
			At:       s.now,
			SizeBTC:  float64(size) / float64(model.Scale),
			TouchPx:  float64(l[0].Price) / float64(model.Scale),
			DepthBTC: float64(execcost.TotalDepth(l)) / float64(model.Scale),
		}

		// Arm A: everything on the single venue holding the best price.
		best := l[0].Venue
		var single execcost.Ladder
		for _, lv := range l {
			if lv.Venue == best {
				single = append(single, lv)
			}
		}
		if c, err := execcost.Walk(single, s.side, size); err != nil {
			r.NaiveErr = short(err)
		} else {
			r.NaiveBps = c.SlippageBps
			r.NaiveFee, _ = s.fees.FeeBps(c)
		}

		// Arm B: routed across both venues right now. Doubles as arm C's
		// pessimistic bound (zero replenishment between slices).
		if c, err := execcost.Walk(l, s.side, size); err != nil {
			r.RoutedErr = short(err)
		} else {
			r.RoutedBps = c.SlippageBps
			r.RoutedFee, _ = s.fees.FeeBps(c)
			if c.Filled > 0 {
				r.RoutedKR = float64(c.PerVenue[model.Kraken]) / float64(c.Filled)
			}
		}

		s.rows = append(s.rows, r)

		// Arm C: schedule the slices; they price against future books.
		step := s.horizon / time.Duration(s.slices)
		s.pendings = append(s.pendings, &pending{
			rowIdx: len(s.rows) - 1, size: size, remaining: size,
			nextAt: s.now, step: step, left: s.slices,
		})
	}
}

// advanceSlices executes any arm-C slice whose time has come.
func (s *study) advanceSlices() {
	if len(s.pendings) == 0 {
		return
	}
	live := s.pendings[:0]
	for _, p := range s.pendings {
		for p.left > 0 && !s.now.Before(p.nextAt) {
			l := s.ladder(s.side)
			sliceQty := p.remaining / model.Qty(p.left)
			if p.left == 1 {
				sliceQty = p.remaining // last slice mops up the rounding
			}
			if sliceQty > 0 && len(l) > 0 {
				c, err := execcost.Walk(l, s.side, sliceQty)
				if err != nil {
					p.failed = true
					s.rows[p.rowIdx].SlicedErr = short(err)
					p.left = 0
					break
				}
				p.notional += c.Notional
				fee, _ := s.fees.TakerFee(c)
				p.fee += fee
				// c.SlippageBps is already measured against THIS slice's own
				// touch, so weighting it by size gives the drift-free cost.
				p.localWeighted += c.SlippageBps * float64(sliceQty)
				p.qtyDone += sliceQty
				p.remaining -= sliceQty
			}
			p.left--
			p.nextAt = p.nextAt.Add(p.step)
		}
		if p.left > 0 {
			live = append(live, p)
			continue
		}
		if !p.failed {
			s.settle(p)
		}
	}
	s.pendings = live
}

// settle turns a finished parent order into its slippage-versus-touch figure.
func (s *study) settle(p *pending) {
	r := &s.rows[p.rowIdx]
	filled := p.size - p.remaining
	if filled <= 0 || p.notional <= 0 {
		r.SlicedErr = "nofill"
		return
	}
	avg := p.notional.Float64() / (float64(filled) / float64(model.Scale))
	if r.TouchPx == 0 {
		return
	}
	// Same sign convention as execcost: positive means worse than the touch
	// observed when the parent order started.
	diff := avg - r.TouchPx
	if s.side == model.Bid {
		diff = r.TouchPx - avg
	}
	r.SlicedBps = diff * 10000.0 / r.TouchPx
	if p.qtyDone > 0 {
		r.SlicedLocalBps = p.localWeighted / float64(p.qtyDone)
		// Whatever the arrival benchmark says that the contemporaneous one
		// does not is the market moving under you, not execution quality.
		r.DriftBps = r.SlicedBps - r.SlicedLocalBps
	}
	if p.notional > 0 {
		r.SlicedFee = float64(p.fee) * 10000.0 / float64(p.notional)
	}
}

// finish discards parents that never completed before the tape ended, rather
// than reporting a partial execution as if it were a whole one.
func (s *study) finish() {
	for _, p := range s.pendings {
		s.rows[p.rowIdx].SlicedErr = "tape-ended"
	}
	s.pendings = nil
}

// ---------- Phase 5: book resilience ----------

// resilience measures how fast displayed depth at the touch comes back after
// it is consumed. It is what tells you where inside the arm-C band reality
// sits: books that refill within a slice interval behave like the optimistic
// bound, books that do not behave like the pessimistic one.
type resilience struct {
	last       model.Qty
	peak       model.Qty
	depletedAt time.Time
	inEvent    bool
	Events     int
	Recovered  int
	totalRec   time.Duration
	worst      time.Duration
}

// depletionFrac is how far top-of-book size must fall to count as an event.
const depletionFrac = 0.5

func (r *resilience) observe(now time.Time, l execcost.Ladder) {
	if len(l) == 0 {
		return
	}
	// Size available within one tick of the touch.
	touch := l[0].Price
	var atTouch model.Qty
	for _, lv := range l {
		if lv.Price == touch {
			atTouch += lv.Qty
		}
	}
	if r.last == 0 {
		r.last, r.peak = atTouch, atTouch
		return
	}
	if !r.inEvent {
		if atTouch < model.Qty(float64(r.peak)*depletionFrac) {
			r.inEvent, r.depletedAt = true, now
			r.Events++
		} else if atTouch > r.peak {
			r.peak = atTouch
		}
	} else if atTouch >= r.peak {
		d := now.Sub(r.depletedAt)
		r.Recovered++
		r.totalRec += d
		if d > r.worst {
			r.worst = d
		}
		r.inEvent = false
	}
	r.last = atTouch
}

func (r *resilience) mean() time.Duration {
	if r.Recovered == 0 {
		return 0
	}
	return r.totalRec / time.Duration(r.Recovered)
}

// ---------- output ----------

func report(s *study, rep capture.Replay, runID, out string, fees *execcost.FeeTable) {
	fmt.Printf("run %s: %d records replayed, %d decisions\n", runID, rep.Records, len(s.rows))
	if rep.Gaps > 0 {
		fmt.Printf("WARNING: tape has %d gap(s) losing %d events; %d records skipped near them\n",
			rep.Gaps, rep.GapEvents, s.skipped)
	}
	if rep.Truncated {
		fmt.Println("note: tape's final file was truncated (normal for a live or killed capture)")
	}

	if s.res != nil {
		fmt.Printf("\nBOOK RESILIENCE (top-of-book size falling below %.0f%% of its peak)\n", depletionFrac*100)
		fmt.Printf("  depletion events: %d   recovered: %d\n", s.res.Events, s.res.Recovered)
		if s.res.Recovered > 0 {
			fmt.Printf("  mean recovery: %s   worst: %s\n",
				s.res.mean().Round(time.Millisecond), s.res.worst.Round(time.Millisecond))
			fmt.Println("  -> recovery far below the slice interval means slicing buys little;")
			fmt.Println("     the book was already back before the next slice landed.")
		}
	}

	fmt.Println("\nMEDIAN SLIPPAGE, bps (fees shown separately)")
	fmt.Println("  sliced  = vs arrival touch (implementation shortfall, INCLUDES drift)")
	fmt.Println("  sliced* = vs each slice's own touch (pure execution cost, drift-free)")
	fmt.Println("  drift   = the difference; the market moving, NOT an execution result")
	fmt.Printf("  %-8s %8s %8s %8s %8s %8s %8s  %s\n",
		"size", "naive", "routed", "sliced", "sliced*", "drift", "fee", "n")
	bySize := map[float64][]row{}
	for _, r := range s.rows {
		bySize[r.SizeBTC] = append(bySize[r.SizeBTC], r)
	}
	var keys []float64
	for k := range bySize {
		keys = append(keys, k)
	}
	sort.Float64s(keys)
	for _, k := range keys {
		rs := bySize[k]
		fmt.Printf("  %-8.3f %8s %8s %8s %8s %8s %8s  %d\n", k,
			med(rs, func(r row) (float64, bool) { return r.NaiveBps, r.NaiveErr == "" }),
			med(rs, func(r row) (float64, bool) { return r.RoutedBps, r.RoutedErr == "" }),
			med(rs, func(r row) (float64, bool) { return r.SlicedBps, r.SlicedErr == "" }),
			med(rs, func(r row) (float64, bool) { return r.SlicedLocalBps, r.SlicedErr == "" }),
			med(rs, func(r row) (float64, bool) { return r.DriftBps, r.SlicedErr == "" }),
			med(rs, func(r row) (float64, bool) { return r.RoutedFee, r.RoutedErr == "" }),
			len(rs))
	}

	fmt.Println("\nARM C IMPACT BAND: the true sliced cost lies between the 'sliced'")
	fmt.Println("column (full replenishment) and the 'routed' column (none).")
	fmt.Println("EVERY figure is a LOWER BOUND: displayed depth hides iceberg size,")
	fmt.Println("and makers pull quotes when they see aggression.")

	if !fees.AllVerified() {
		fmt.Println("\n*** UNVERIFIED FEE INPUT — NO HEADLINE NUMBER MAY BE PUBLISHED ***")
		for _, c := range fees.Caveats() {
			fmt.Println("  " + c)
		}
	}
	fmt.Printf("\nwrote %s\n", out)
}

func med(rs []row, pick func(row) (float64, bool)) string {
	var v []float64
	for _, r := range rs {
		if x, ok := pick(r); ok {
			v = append(v, x)
		}
	}
	if len(v) == 0 {
		return "-"
	}
	sort.Float64s(v)
	return strconv.FormatFloat(v[len(v)/2], 'f', 2, 64)
}

func writeCSV(path string, rows []row) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	// A failed close on a file we just wrote can mean truncated output, so it
	// is reported rather than discarded — unless something worse came first.
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	w := csv.NewWriter(f)
	if err := w.Write([]string{
		"at", "size_btc", "touch_px", "displayed_depth_btc",
		"naive_bps", "naive_fee_bps", "routed_bps", "routed_fee_bps",
		"routed_kraken_frac", "sliced_bps", "sliced_local_bps", "drift_bps",
		"sliced_fee_bps", "naive_err", "routed_err", "sliced_err",
	}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write([]string{
			r.At.Format(time.RFC3339Nano),
			ff(r.SizeBTC), ff(r.TouchPx), ff(r.DepthBTC),
			ff(r.NaiveBps), ff(r.NaiveFee), ff(r.RoutedBps), ff(r.RoutedFee),
			ff(r.RoutedKR), ff(r.SlicedBps), ff(r.SlicedLocalBps), ff(r.DriftBps),
			ff(r.SlicedFee), r.NaiveErr, r.RoutedErr, r.SlicedErr,
		}); err != nil {
			return err
		}
	}
	// Flush explicitly, not via defer: a deferred flush runs AFTER the return
	// value is evaluated, so a failure on the last write would be lost.
	w.Flush()
	return w.Error()
}

func ff(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func short(err error) string {
	switch {
	case errors.Is(err, execcost.ErrInsufficientDepth):
		return "nodepth"
	case errors.Is(err, execcost.ErrEmptyLadder):
		return "nobook"
	case errors.Is(err, execcost.ErrOverflow):
		return "overflow"
	case errors.Is(err, execcost.ErrUnsorted):
		return "unsorted"
	}
	return "err"
}

func parseSizes(s string) ([]model.Qty, error) {
	var out []model.Qty
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f, err := strconv.ParseFloat(part, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("bad size %q", part)
		}
		out = append(out, model.Qty(f*float64(model.Scale)))
	}
	if len(out) == 0 {
		return nil, errors.New("no sizes given")
	}
	return out, nil
}

func resolveRun(dir, want string) (string, error) {
	runs, err := capture.Runs(dir)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "", fmt.Errorf("no capture runs in %s", dir)
	}
	if want != "" {
		for _, r := range runs {
			if r.RunID == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("run %s not found in %s", want, dir)
	}
	return runs[len(runs)-1].RunID, nil // newest
}
