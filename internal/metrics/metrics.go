// Package metrics holds the process-wide operational counters. A full buffer,
// a resync, a skipped checksum — every defined failure mode is surfaced here
// rather than silently swallowed (spec §4.4).
package metrics

import "sync/atomic"

var (
	// DroppedEvents counts feed→engine hand-offs dropped because the bounded
	// channel was full (defined failure mode: the engine is behind).
	DroppedEvents atomic.Int64
	// Resyncs counts venue resyncs (crossed book, checksum mismatch, dropped
	// update, disconnect-triggered rebuilds requested by the engine).
	Resyncs atomic.Int64
	// ChecksumMismatches counts real Kraken checksum failures at full depth.
	ChecksumMismatches atomic.Int64
	// ChecksumSkippedThin counts checksum comparisons skipped because a side
	// held fewer than 10 levels (thin-book guard, spec §6.4 — prevents the
	// resync storm right after a fresh snapshot).
	ChecksumSkippedThin atomic.Int64
	// Reconnects counts websocket session re-establishments.
	Reconnects atomic.Int64
	// CrossVenueCrosses counts publishes where the consolidated book was
	// locked/crossed ACROSS venues — normal and transient (no SIP in
	// crypto), flagged but never resynced (spec §7).
	CrossVenueCrosses atomic.Int64
)
