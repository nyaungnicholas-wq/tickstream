# TickStream

A real-time consolidated crypto order book in Go: ingests Level-2 feeds from
Coinbase and Kraken, reconstructs each venue's book, merges them into one
consolidated best-bid/offer, computes microstructure signals, and serves a
consistent top-of-book snapshot — with an honestly-benchmarked **end-to-end
apply latency** and a wait-free read path.

> Full README lands at M5. This stub tracks the build (currently: M0).
