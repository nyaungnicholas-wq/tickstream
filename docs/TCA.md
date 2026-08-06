# Execution-cost study (TCA) on consolidated crypto depth

What does it actually cost to fill a market order in BTC-USD, and does routing
across venues or slicing over time make it cheaper?

This is a **measurement** study, not an optimizer. Every "execution algo"
project built on simulated fills answers a question its own assumptions
decided in advance. The design here splits the question into what a recorded
tape can prove and what it cannot, and only claims the first.

## What is measurable, and what is not

| | |
|---|---|
| **Measurable, no assumptions** | The cost of walking real recorded depth for size Q at time *t*, on one venue vs split across two, net of each venue's fee tier. Arithmetic over recorded data. |
| **Measurable, no assumptions** | Book resilience: how long displayed size at the touch takes to come back after it is consumed. |
| **NOT measurable, ever** | How the book would have reacted to *your* order. It isn't in the tape. |

The third row is why arm C is reported as a **band** rather than a number.

## The three arms

- **A — naive:** the whole order, immediately, on the single venue holding the
  best price.
- **B — routed:** the whole order, immediately, across both venues' real depth.
- **C — sliced:** N slices over a horizon, each one routed.

## The impact band (no free parameters)

Rather than invent a market-impact coefficient and let it decide the answer,
arm C is bracketed by two bounds that are *both* pure measurement:

- **Optimistic** = arm C as recorded — the book fully replenishes between
  slices, so each slice pays what the tape actually shows.
- **Pessimistic** = arm B — zero replenishment, so every slice competes for the
  same liquidity as one big order.

The truth is between them. The **resilience measurement says where**: if the
book refills much faster than the slice interval, reality sits near the
optimistic bound; if it doesn't, near the pessimistic one.

## Caveats that ship with every number

1. **Every figure is a LOWER BOUND on real cost.** L2 shows resting limit
   orders only. Hidden and iceberg size is invisible, and makers pull quotes
   when they see aggression. A real order pays more than this — never less.
2. **Feed granularity differs across venues.** Coinbase `level2_batch` is
   batched (~50 ms); Kraken v2 `book` is per-update. The consolidated snapshot
   therefore carries real skew, which is an error term on the routed arm.
3. **Arm C is reported two ways, because the arrival benchmark is
   drift-contaminated.** Measured against the arrival touch (implementation
   shortfall), any price move over the horizon lands in arm C's number and not
   in arm A's or B's, and over a few minutes that drift dominates completely.
   So each slice is *also* scored against the touch prevailing at its own
   moment, size-weighted — a drift-free measure of pure execution cost. The
   difference between the two is reported as `drift` and is explicitly not an
   execution result. Compare arms on the drift-free column only.

## Fees decide the answer

A cross-venue dislocation of a few dollars on a $60k asset is ~1 bp. A single
taker fee at the entry tier is ~80 bp. The harness therefore refuses to print a
headline verdict while any fee input is unverified.

- **Kraken** — [kraken.com/features/fee-schedule](https://www.kraken.com/features/fee-schedule),
  read 2026-08-06. Entry tier 0.40% maker / **0.80% taker**. Verified.
- **Coinbase Exchange** — [exchange.coinbase.com/fees](https://exchange.coinbase.com/fees),
  read 2026-08-06. Entry tier 0.40% maker / **0.60% taker**. Verified.

Getting the *product* right matters as much as the tier: Coinbase **Advanced
Trade** charges 1.20% taker at the entry tier while **Exchange** — the
institutional product TickStream actually connects to — charges 0.60%. Using
the wrong ladder doubles every Coinbase figure and inverts the routing story.
Note that at the entry tier Coinbase Exchange is *cheaper* on taker fees than
Kraken.

## Pilot result (~11 minutes of tape — NOT the study)

Enough data to prove the pipeline, nowhere near enough to conclude anything.

```
size(BTC)   naive   routed   sliced  sliced*    drift      fee    n
0.100        0.00     0.00    -1.28     0.00    -1.28    60.00   28
0.500        0.10     0.10    -1.28     0.04    -1.28    60.00   28
1.000        0.36     0.26    -1.28     0.08    -1.28    69.82   28
2.000        0.60     0.34    -1.15     0.13    -1.28    74.91   28
5.000        1.33     0.66    -0.98     0.30    -1.28    76.91   28

book resilience: 6 depletion events, mean recovery 13.2s, worst 31.7s
```

`sliced` is arrival-benchmarked; `sliced*` is drift-free. Note that `drift` is
**−1.28 bps at every size** — identical regardless of hypothetical order size,
which is exactly what a pure market move must look like. An execution effect
would scale with size. That constancy is a useful internal check that the
decomposition is doing what it claims.

Three things are visible, and all are the *shape* the full study should confirm
or refute:

1. **Execution cost is 0.00–1.33 bps. Fees are 60–77 bps.** Routing and slicing
   optimize something ~60–100× smaller than the fee being paid. This matches the
   standing TickStream finding that the persistent cross-venue cross sits inside
   the fee band.
2. **The arms do order correctly once drift is removed:** naive 1.33 > routed
   0.66 > sliced\* 0.30 at 5 BTC. Routing roughly halves the cost, slicing
   roughly halves it again. Both are real — and both are rounding errors next to
   the fee.
3. **The blended fee *rises* with size**, 60 → 77 bps, because small orders fill
   entirely on cheaper Coinbase Exchange (0.60% taker) and larger ones spill onto
   Kraken (0.80%). That size-dependent fee effect is an order of magnitude bigger
   than anything routing does to the price.

The resilience number (mean 13.2 s recovery vs a 30 s slice interval) says the
book was already back before the next slice landed — i.e. reality sits near the
**optimistic** end of the arm-C band, and slicing has little room to help.

## Running it

Record (runs supervised, restarts on crash, stops before filling the disk):

```bash
pwsh -File scripts/capture.ps1 -Dir C:\tickstream-capture -Depth 1000
```

`-depth 1000` is not optional. The engine truncates both books to the
subscribed depth, and the depth on the tape is a hard ceiling on the order size
the study can ever price — a depth-10 BTC book is a couple of BTC.

Inspect a tape's integrity:

```bash
go run ./cmd/tapestat -dir C:\tickstream-capture
```

Run the study:

```bash
go run ./cmd/tcastudy -dir C:\tickstream-capture -sizes 0.1,0.5,1,2,5,10 -every 1m -horizon 5m -slices 6 -out tca.csv
```

Chart it:

```bash
python scripts/analyze_tca.py tca.csv
```

## Tape integrity

Recording drops are a defined, *localized* failure: the recorder writes a gap
marker into the stream at the point of loss, so a replay knows exactly where
the hole is instead of only that one exists. `tcastudy` drops any in-flight
sliced order that crosses a gap rather than pricing it against a book that
skipped updates, and `tapestat` prints a warning for any tape with gaps.
