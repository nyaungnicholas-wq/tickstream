# TickStream

**Consolidated crypto order book in Go with measured end-to-end apply latency
and a wait-free read path.**

[![CI](https://github.com/nyaungnicholas-wq/tickstream/actions/workflows/ci.yml/badge.svg)](https://github.com/nyaungnicholas-wq/tickstream/actions)
![Go](https://img.shields.io/badge/go-1.24+-00ADD8?logo=go)
![License](https://img.shields.io/badge/license-MIT-blue)

TickStream ingests live Level-2 order-book feeds from **Coinbase Exchange**
and **Kraken**, reconstructs each venue's book correctly (snapshot +
incremental, absolute-size semantics, checksum-validated), merges them into
one consolidated best-bid/offer — a crypto "NBBO" — computes microstructure
signals (order-book imbalance, size-weighted mid), and serves a consistent
top-of-book snapshot to any number of readers.

**The headline number, measured honestly on the machine below:**

> On an Intel Core i5-1038NG7 @ 2.00 GHz (8 cores), 16 GB RAM, macOS
> (Darwin 25.5.0), Go 1.26.4, GOMAXPROCS=8, idle, `-race` off, open-loop
> replay at 5,000 events/s with intended-time measurement:
> **end-to-end apply latency p50 = 7.4 µs · p99 = 40.8 µs · max = 4.09 ms**
> over 200,000 samples. The read path is **wait-free, p50 ≈ 41 ns**
> (coordinated-omission correction is demonstrative-only there — a wait-free
> load cannot stall).

Full numbers, tail analysis, and methodology: [docs/benchmarks.md](docs/benchmarks.md).

## What it does — and guarantees

- **Correct reconstruction:** update quantities are absolute (overwrite,
  never add); `qty == 0` deletes a level; deletes of absent levels are
  no-ops; updates before a snapshot are ignored.
- **Integrity-checked:** every Kraken message's CRC32 is verified against the
  local book (the known-good vector `3310070434` is a unit test); a mismatch
  or a single-venue crossed book triggers a full resync, never a patch-up.
- **Consolidated BBO:** max-bid / min-ask across venues with size aggregation
  at shared prices and venue attribution.
- **Honest measurement:** every latency number ships with hardware and
  methodology disclosure — the harness *refuses* to emit results with empty
  disclosure fields or from a `-race` build.

## Architecture

```
   Coinbase WS                         Kraken WS
 (level2_batch)                       (v2 "book")
      │                                   │
      ▼                                   ▼
┌──────────────┐                    ┌──────────────┐
│ feed/coinbase│  goroutine #1      │ feed/kraken  │  goroutine #2
│  read→decode │                    │  read→decode │
│  →normalize  │                    │  →normalize  │
└──────┬───────┘                    └──────┬───────┘
       │   chan Event (BUFFERED cap=4096,  │
       │   non-block-or-drop + resync)     │
       └─────────────┬─────────────────────┘
                     ▼
        ┌────────────────────────────┐
        │   ENGINE  (single writer)  │  goroutine #3 — owns ALL book state
        │  book[CB]     book[KR]     │
        │  consolidator → signals    │
        │  build FRESH immutable     │
        │  Snapshot                  │
        │  atomic.Pointer.Store() ───┼──────┐
        └────────────────────────────┘      ▼   atomic.Load()  (WAIT-FREE)
                                  ┌─────────────────────────┐
                                  │  READERS (N goroutines) │
                                  └─────────────────────────┘
```

The race-sensitive shared state is the venue books and consolidated view —
and **exactly one goroutine (the engine) ever mutates them**, so write-write
races are impossible by construction. Readers never take a lock: the engine
publishes a **freshly-allocated immutable snapshot** through an
`atomic.Pointer`, and a reader's `Load()` is one pointer read.

Two non-obvious failure modes are designed in, not patched later:

- **Immutability invariant** (`-race` cannot catch this): an old `Load()`
  must never change under a reader, so the engine never reuses a backing
  array/slice/map across publishes — enforced by concurrent assertion tests.
- **Bounded hand-off**: the feed→engine channel is bounded (4096) and the
  hand-off never blocks — a full buffer drops the frame, increments a metric,
  and forces that venue to resync. Blocking would stall the websocket read
  loop, starve pings, and death-spiral the connection.

Details: [docs/architecture.md](docs/architecture.md).

## Quick start

```sh
make build        # → bin/tickstreamd
make run          # live consolidated NBBO for BTC, updated every second
make test         # unit + concurrency tests, with -race
make bench-e2e    # the honest apply-latency harness (+ latency.csv)
```

**Windows (no make):**

```powershell
go build ./...
go test ./...
go run ./cmd/tickstreamd   # equivalent of `make run`
```

Sample live output:

```
NBBO bid=59951.11@CB ask=59948.1@KR mid=59949.60 wmid=59949.52 imb=+0.056 spread=-3.01  [XVENUE-CROSSED]  applyLat=63.947µs drops=0 resyncs=0
```

(Yes, that consolidated book is *crossed* — see the honesty notes below.)

## Order-book reconstruction

Universal rules (they are also the unit-test matrix): absolute-size
overwrite, `qty 0` = delete, delete-of-absent = no-op, gap/desync ⇒ discard
and resync from a fresh snapshot. Kraken integrity = per-message CRC32 over
the top-10 of each side, computed over the **exact wire decimal strings**
(never a float round-trip), with a **thin-book guard**: sides holding < 10
levels skip the comparison (metric, not resync) so a fresh thin snapshot can
never storm.

**Data-structure honesty:** each side is a hash map plus a price slice sorted
worst→best — the best price lives at the *end*. Best-of-book reads are O(1);
**delete-of-best is O(1)** (pop + map delete; the layout moves the classic
O(n) delete-best trap off the hot path); updating an *arbitrary* level is
O(log P) search + O(P) worst-case memmove, cheapest near the top where feed
activity concentrates and most expensive at the deepest level
(`BenchmarkApplyDeepInsert` measures the worst case rather than hiding it:
~4.5 µs at 10k levels).

## Consolidation + signals

`bestBid = max` over ready venues, `bestAsk = min`, sizes aggregated across
venues at the shared best price, tie-broken by size then stable venue order
(L2 carries no per-order timestamps, so true time priority is unknowable —
stated, not fudged).

Live signals (v1): **order-book imbalance** (both conventions, signed and
fraction — `signed = 2·fraction − 1`) and the **size-weighted mid**
(cross-multiplied: `(Pb·Qa + Pa·Qb)/(Qa+Qb)` — same-side multiplication is a
book VWAP, not a fair-value estimator).

**Honesty notes (read these before quoting the signals):**

- The weighted mid is **not a martingale**: cancel part of the best ask and
  it moves down, which is intuitively wrong — that flaw is the entire reason
  the **Stoikov micro-price** exists. v1 *explains* the micro-price
  ([docs/microprice.md](docs/microprice.md)) and does not implement it; the
  offline fit → live lookup is an explicit stretch goal.
- **There is no SIP in crypto.** We build the consolidated book ourselves
  from feeds with independent latency, so the consolidated view can sit
  locked or *crossed across venues* — normal, flagged
  (`[XVENUE-CROSSED]`), and never resynced. Live observation: Coinbase and
  Kraken held a persistent ~$2–3 cross on ~$60k BTC — too small to
  arbitrage inside the fee band. The opposite holds **within** one venue,
  where a crossed book means *our* book is corrupt ⇒ resync.

## Execution-cost study (TCA)

What does it actually cost to fill a market order, and does routing across
venues or slicing over time make it cheaper? This is the question the
"too small to arbitrage inside the fee band" observation above raises, answered
with measurement instead of a simulator.

Three arms are priced against a recorded tape: **naive** (whole order, one
venue), **routed** (whole order, both venues' real depth), **sliced** (TWAP).
The design constraint is that a tape can never show how the book would have
reacted to *your* order — so rather than invent an impact coefficient, the
sliced arm is reported as a **band between two bounds that are both pure
measurement**, and the book-resilience number says where inside it reality
sits. Slippage is also decomposed into execution cost and price drift, because
over a few minutes drift otherwise dominates the comparison entirely.

Every figure is a **lower bound**: displayed depth omits hidden and iceberg
size, and makers pull quotes when they see aggression.

```bash
pwsh -File scripts/capture.ps1 -Dir C:\tickstream-capture -Depth 1000  # record
go run ./cmd/tapestat  -dir C:\tickstream-capture                      # tape integrity
go run ./cmd/tcastudy  -dir C:\tickstream-capture -out tca.csv         # the study
python scripts/analyze_tca.py tca.csv                                  # chart it
```

Method, caveats, fee provenance and results: **[docs/TCA.md](docs/TCA.md)**.

## Performance / benchmarks

Methodology first, numbers second — that order is the point.

- **Headline = end-to-end apply latency** (event decoded → snapshot
  published), driven **open-loop** from a replayed feed at a target rate and
  measured from each event's **intended** arrival time (the wrk2 approach).
  This path can queue and stall (single writer, bounded channel), so
  **coordinated-omission-aware measurement is load-bearing** — a stall is
  charged to every event it delays, not hidden by a stopped clock.
- **Read path is a wait-free footnote:** one atomic pointer load, p50 ≈ 41 ns
  (timer overhead included). An atomic load never blocks, so there is no
  stall for CO machinery to correct — and it is deliberately not advertised
  as "sub-millisecond".
- Custom harness (not `testing.B` — a distribution, not one `ns/op`),
  monotonic clock only, warm-up then reset, HdrHistogram with checked
  recording errors, full tail + max reported, CSV + HdrHistogram
  plotter-format output, and a **mandatory hardware disclosure the harness
  enforces** (empty field or `-race` build ⇒ refuses to print).

| metric | p50 | p99 | p99.9 | max |
|---|---:|---:|---:|---:|
| apply (headline) | 7.4 µs | 40.8 µs | 104 µs | 4.09 ms |
| read (footnote) | 41 ns | 44 ns | 66 ns | 150 µs |

The p99.99/max tail is GC + scheduler — analyzed, not excused, in
[docs/benchmarks.md](docs/benchmarks.md).

## Observability

The 1s status line shows the NBBO, both signals, venue attribution, apply
latency, and drop/resync counters. Internally: `DroppedEvents`, `Resyncs`,
`ChecksumMismatches`, `ChecksumSkippedThin`, `CrossVenueCrosses`,
`Reconnects` (`internal/metrics`). Every defined failure mode increments a
counter; nothing fails silently.

## Development

```sh
make help   # all targets
make lint   # golangci-lint v2 (govet+shadow, staticcheck, errcheck, …)
make test   # go test -race -buildvcs ./...
```

Layout: `cmd/` holds thin binaries (`tickstreamd`, `tickstream-bench`, the
throwaway M0.5 `probe`); all logic lives in `internal/` (`model`, `book`,
`checksum`, `feed/{coinbase,kraken,wsutil,jsonx}`, `engine`, `consolidate`,
`signals`, `snapshot`, `bench`, `metrics`). To add a venue: implement
`feed.Feed` (decode + subscribe + resync signal) and add its integrity rule
to the engine — the books, consolidator, and snapshot path are venue-agnostic.

## Testing

`testdata/` fixtures drive the decoders offline; the Kraken checksum test
asserts the official vector **3310070434** exactly; the book tests cover the
full apply-semantics matrix including delete-of-current-best; and the
snapshot-immutability tests are *concurrent assertion* tests, because the
race detector cannot see that logic race. CI runs vet, lint, `-race` tests,
and a compile-only bench smoke (never recorded as results).

## Limitations / not handled (v1 — stated, not hidden)

L3/per-order books · matching engine · more than 2 venues · FIX ·
authenticated feeds · clock-skew correction across venues · live micro-price
(offline fit is a stretch goal) · capture/replay tooling (M6) · the brief
staleness window between a dropped frame and its resync snapshot.

## Roadmap (v2 ideas)

Capture/replay for reproducible benches · offline micro-price fit → live
`g(I,S)` lookup · more venues (Binance `U`/`u` straddle, OKX `seqId`) ·
seqlock/ring-buffer read-path comparison · Rust hot-path rewrite benchmarked
head-to-head on the same harness · per-symbol engine sharding.

## License

MIT — see [LICENSE](LICENSE).
