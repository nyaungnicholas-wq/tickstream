// Package feed defines the venue-feed contract shared by the adapters.
package feed

import (
	"context"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// Feed is one venue's ingestion adapter: it owns the websocket session,
// decodes frames in its own goroutine, and emits normalized Events on out.
// The hand-off to out MUST be non-blocking (bounded channel, drop + metric +
// resync on full — spec §4.4); a Feed never blocks its read loop.
type Feed interface {
	Run(ctx context.Context, out chan<- model.Event) error
	Venue() model.Venue
	// RequestResync drops the current session so the venue reconnects and
	// delivers a fresh snapshot. Non-blocking and idempotent; called by the
	// engine on crossed books, checksum mismatches, and dropped updates.
	RequestResync()
}
