# Benchmarks — tracked results

> Reproduce: `make bench-e2e` (or `go run ./cmd/tickstream-bench`). The
> harness **refuses to emit results** if any disclosure field is empty or the
> binary was built with `-race`. All numbers below are from a real run of the
> committed code; none are hand-edited.

## The honest result (2026-07-01)

> On an **Intel(R) Core(TM) i5-1038NG7 CPU @ 2.00GHz** (8 cores), **16 GB
> RAM**, **macOS (Darwin 25.5.0)**, **Go go1.26.4**, GOMAXPROCS=8, idle
> machine, `-race` off, open-loop replay at **5,000 events/s** with
> intended-time measurement: **end-to-end apply latency p50 = 7.4 µs,
> p99 = 40.8 µs, max = 4.09 ms** over 200,000 samples. The read path is
> **wait-free, p50 ≈ 41 ns** (CO correction demonstrative only — a wait-free
> load cannot stall).

## HEADLINE — end-to-end apply latency

`event-received → snapshot-published`, through the full real path: book
apply, Kraken truncate, crossed/locked guard, CRC32 checksum verification,
fresh immutable snapshot build, atomic publish. Driven **open-loop** from a
deterministic replayed feed; each latency is measured from the event's
**scheduled** arrival time (the wrk2 approach), so an engine stall is charged
to *every* event it delays — this path can queue, which is exactly why
coordinated-omission handling is load-bearing here.

| percentile | latency |
|---:|---:|
| p50 | 7.411 µs |
| p90 | 15.095 µs |
| p99 | 40.767 µs |
| p99.9 | 104.127 µs |
| p99.99 | 2.144 ms |
| max | 4.092 ms |

*Samples: 200,000 (+20,000 warm-up discarded). CO mode: `intended`
(open-loop); `-mode corrected` re-runs it closed-loop with
`RecordCorrectedValue` backfill for comparison.*

**Reading the tail honestly:** the p50→p99 band (7→41 µs) is the engine
itself (apply + checksum + fresh snapshot allocation). The p99.99/max
excursions into the milliseconds are GC pauses plus macOS scheduler
preemption on a passively-cooled laptop — the fresh-allocation-per-publish
immutability invariant (§4.3) deliberately trades steady allocation pressure
for correctness, and the tail is where that trade shows. Understanding that
tail is the point of reporting it.

## Footnote — read path (wait-free)

One `atomic.Pointer.Load()` plus reads of the consolidated top-of-book and
signals:

| percentile | latency |
|---:|---:|
| p50 | 41 ns |
| p99 | 44 ns |
| p99.9 | 66 ns |
| max | 149.6 µs |

*Samples: 1,000,000. Each sample includes ~2× `time.Now()` timer overhead —
comparable to the operation itself — so treat these as an upper bound. An
atomic load never blocks and never queues; there is no stall for
coordinated-omission machinery to correct on this path, which is why this
number is a footnote and not the headline. It is deliberately **not**
advertised as "sub-millisecond" — that would be true and useless.*

## Methodology checklist

- Custom load loop, not `testing.B` (a distribution, not one `ns/op`).
- Monotonic clock only (`time.Now`/`time.Since`; no
  `Round/Truncate/UTC/Local` on measured timestamps).
- Warm-up phase (20k events), then `Reset()` before recording.
- HdrHistogram `New(1, 60_000_000_000, 3)`; every `RecordValue` error checked
  (out-of-range samples abort the run rather than silently vanishing).
- DOC-CHECKED against pinned hdrhistogram-go v1.2.0: percentile accessor is
  `ValueAtPercentile` (0–100 scale); CO backfill is `RecordCorrectedValue`;
  plotter dump is `PercentilesPrint(w, 1, 1.0)`.
- Full tail reported (p50/p90/p99/p99.9/p99.99 + max); never a bare mean.
- Outputs: `latency.csv` + HdrHistogram plotter format on stdout (paste into
  the free online HdrHistogram plotter for the chart).
