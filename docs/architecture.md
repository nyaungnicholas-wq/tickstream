# TickStream — Architecture

## Data flow

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
       │  chan Event (BUFFERED cap=4096,   │
       │  non-block-or-drop + resync)      │
       └─────────────┬─────────────────────┘
                     ▼
        ┌────────────────────────────┐
        │   ENGINE  (single writer)  │  goroutine #3 — owns ALL book state
        │  book[CB]  book[KR]        │
        │  consolidator → signals    │
        │  build FRESH immutable     │
        │  Snapshot (+apply-ts)      │
        │  atomic.Pointer.Store() ───┼───────┐
        └────────────────────────────┘       ▼  atomic.Load()  (WAIT-FREE)
                                   ┌─────────────────────────┐
                                   │ READERS (N goroutines)  │
                                   │ • 1s status printer     │
                                   │ • bench harness         │
                                   └─────────────────────────┘
```

One sentence: two feed goroutines decode and normalize venue messages onto a
bounded channel → one engine goroutine applies them to per-venue books,
consolidates to a cross-venue BBO, computes signals, stamps an apply
timestamp, and publishes a freshly-allocated immutable snapshot via an atomic
pointer → any number of reader goroutines load the snapshot wait-free.

## Concurrency model — the four rules

1. **Goroutine-per-feed.** Each venue feed decodes in its own goroutine and
   never touches another feed's book.
2. **Single writer.** Exactly one goroutine (the engine) mutates book state
   and the consolidator. Write-write races are impossible by construction.
   The single writer is a per-shard throughput ceiling (one core); you scale
   by sharding symbols across independent single-writers, never by adding
   locks.
3. **Atomic-snapshot fan-out.** The engine publishes a fresh immutable
   `*Snapshot` via `atomic.Pointer.Store`; readers `Load()` wait-free.
   Why not a channel (a mutex-guarded queue — wrong shape for broadcasting
   the latest value) or an `RWMutex` (writer-preferring; every `RLock` writes
   a shared cache line, so it does not scale with readers)?
4. **Bounded hand-off with an explicit full-buffer policy** (below).

## The §4.3 invariant — snapshot immutability (`-race` cannot catch this)

> A published `Snapshot` — and **every slice and map it transitively owns** —
> is immutable and **freshly allocated per publish**. The engine MUST NOT
> retain any reference into a published snapshot, and MUST NOT reuse a
> backing array, slice, or map across publishes. The Venues map and every
> Level it contains are fresh allocations each publish.

The atomic swap protects the *pointer*, not the bytes an old pointer still
references: if the engine reused a backing array, a reader holding an older
`Load()` would see torn data with zero data-race reports. Enforced by
concurrent assertion tests in `internal/snapshot` and `internal/engine`
(writer republishing in a loop; readers asserting an old `Load()` never
changes), and by the code-review rule that `publish()` allocates everything
fresh.

## The §4.4 policy — bounded hand-off (read-loop ↔ ping deadlock)

`coder/websocket`'s `Ping` needs the read loop running to receive the pong.
If `onMessage` ever blocked on a full engine channel, the read loop would
block → pings starve → the connection dies → resync → which needs the engine
→ death spiral. Therefore:

- the feed→engine channel is **bounded (cap = 4096)**;
- the hand-off is **non-blocking-with-drop**: on a full buffer the frame is
  dropped, `metrics.DroppedEvents` increments, and the feed requests its own
  **resync** (a dropped update makes the book suspect — rebuild from a fresh
  snapshot rather than silently applying a gap);
- a full buffer is a **defined failure mode**, surfaced as a metric — never
  an invisible block. Between the drop and the fresh snapshot the stale book
  remains readable; that brief window is accepted and documented.

## Sequencing / integrity / resync per venue

| Venue | Integrity mechanism | On failure |
|---|---|---|
| Coinbase Exchange `level2_batch` | No per-message seq; TCP order + heartbeat liveness | reconnect → fresh snapshot → rebuild |
| Kraken v2 `book` | CRC32 over top-10 per side, every message | checksum mismatch or disconnect → resubscribe → fresh snapshot → rebuild |

Resync procedure (both venues bootstrap from the WS snapshot; no REST step):
mark book not-ready (consolidator ignores the venue) → drop the session →
reconnect with backoff → resubscribe → fresh snapshot rebuilds → ready.

**Thin-book guard (§6.4):** the Kraken checksum is compared only when both
sides hold ≥ 10 levels; thinner books skip the comparison (surfaced via
`metrics.ChecksumSkippedThin`) so a fresh thin snapshot can never trigger a
resync storm.

**Crossed books:** within ONE venue, `bid >= ask` means our book is corrupt →
resync. ACROSS venues, a locked/crossed consolidated book is normal and
transient (there is no SIP in crypto; feeds have independent latency) → flag
`CrossVenueCrossed`, never resync. Live observation (2026-07-01): Coinbase
and Kraken sat in a persistent ~$2–3 cross on ~$60k BTC — economically
unarbitrageable inside the fee band, and exactly why the flag exists.

## M0.5 connectivity gate — decision record (2026-07-01)

Probe run (`cmd/probe`, throwaway) against both venues, **no authentication**:

| Venue | Endpoint | Result |
|---|---|---|
| Coinbase Exchange | `wss://ws-feed.exchange.coinbase.com`, channel `level2_batch` | **PASS** — full snapshot received with no auth (13,359 bids / 28,075 asks) |
| Kraken | `wss://ws.kraken.com/v2`, channel `book`, depth 10 | **PASS** — snapshot + checksum received with no auth |

**Decision: v1 is a TWO-VENUE build (Coinbase + Kraken).**

Gate findings that bind the adapters:

1. **Coinbase snapshot frames exceed 1 MB.** The full BTC-USD `level2_batch`
   snapshot (~41k levels) blows through a 1 MB `SetReadLimit` with
   `websocket: message too big` — a client-side limit that masquerades as a
   feed failure. The Coinbase adapter sets a 64 MB read limit. Kraken at
   depth 10 is tiny; 1 MB suffices.
2. Kraken v2 `book` delivers `checksum` on every snapshot/update as
   documented (verified live: 0 mismatches over multi-minute runs).
3. Coinbase sends a `"subscriptions"` ack frame first, then `"snapshot"`.

## Apply-latency measurement points (§9.1)

The headline clock starts in the feed goroutine **just after the websocket
read returns and the frame is decoded** (`Event.Received`, a monotonic
`time.Time`) and stops **just after `atomic.Pointer.Store`**. Queue wait on
the bounded channel is inside the window — that is the point: this path can
stall, so coordinated-omission-aware measurement is load-bearing here and
demonstrative-only on the wait-free read.
