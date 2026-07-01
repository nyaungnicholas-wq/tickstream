# TickStream — Architecture

## M0.5 connectivity gate — decision record (2026-07-01)

Probe run (`cmd/probe`, throwaway) against both venues, **no authentication**:

| Venue | Endpoint | Result |
|---|---|---|
| Coinbase Exchange | `wss://ws-feed.exchange.coinbase.com`, channel `level2_batch` | **PASS** — full snapshot received with no auth (13,359 bids / 28,075 asks) |
| Kraken | `wss://ws.kraken.com/v2`, channel `book`, depth 10 | **PASS** — snapshot + checksum received with no auth |

**Decision: v1 is a TWO-VENUE build (Coinbase + Kraken).**

Gate findings that bind later milestones:

1. **Coinbase snapshot frames exceed 1 MB.** The full BTC-USD `level2_batch`
   snapshot (~41k levels) blows through a 1 MB `SetReadLimit` with
   `websocket: message too big` — which looks like a failure but is purely a
   client-side limit. The Coinbase adapter (M3) must set a much larger read
   limit (we use 64 MB). Kraken at depth 10 is tiny; 1 MB is fine there.
2. Kraken v2 `book` delivers `checksum` on every snapshot/update as documented.
3. Coinbase sends a `"subscriptions"` ack frame first, then `"snapshot"`.

*(The rest of this document — data flow, concurrency model, §4.3 immutability
invariant, §4.4 bounded hand-off, sequencing/resync — is filled in at M3–M5.)*
