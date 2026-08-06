package coinbase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/feed/wsutil"
	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// URL is the Coinbase Exchange websocket feed (gate-verified public/no-auth).
const URL = "wss://ws-feed.exchange.coinbase.com"

// readLimit must exceed the full-book snapshot frame size: the BTC-USD
// snapshot measured >1 MB at the M0.5 gate (~41k levels), so 1 MB would kill
// the connection with "message too big" on every connect.
const readLimit = 64 << 20

// Feed is the Coinbase Exchange level2_batch adapter.
type Feed struct {
	symbol string
	resync chan struct{}
}

// New returns a Coinbase feed for one product (e.g. "BTC-USD").
func New(symbol string) *Feed {
	return &Feed{symbol: symbol, resync: make(chan struct{}, 1)}
}

// Venue implements feed.Feed.
func (f *Feed) Venue() model.Venue { return model.Coinbase }

// RequestResync implements feed.Feed: non-blocking, coalescing signal.
func (f *Feed) RequestResync() {
	select {
	case f.resync <- struct{}{}:
	default: // a resync is already pending
	}
}

// Run implements feed.Feed. It reconnects forever until ctx is canceled.
func (f *Feed) Run(ctx context.Context, out chan<- model.Event) error {
	subs := [][]byte{
		[]byte(fmt.Sprintf(`{"type":"subscribe","product_ids":[%q],"channels":["level2_batch"]}`, f.symbol)),
		// matches: the public trade tape. The book alone cannot say what
		// actually traded, so realized VWAP and market impact are unmeasurable
		// without it. Trade frames carry no book levels and never touch book
		// state — they exist to be recorded.
		[]byte(fmt.Sprintf(`{"type":"subscribe","product_ids":[%q],"channels":["matches"]}`, f.symbol)),
		// heartbeat: liveness only (channels go quiet on low-volume products).
		[]byte(fmt.Sprintf(`{"type":"subscribe","channels":[{"name":"heartbeat","product_ids":[%q]}]}`, f.symbol)),
	}
	onMessage := func(data []byte) error {
		received := time.Now() // apply-latency clock starts here (§9.1)
		ev, ok, err := DecodeMessage(data)
		if err != nil {
			// Tolerate junk/drifted frames without killing the session.
			slog.Warn("coinbase: undecodable frame", "err", err)
			return nil
		}
		if !ok {
			return nil
		}
		ev.Received = received
		// Bounded hand-off (§4.4): NEVER block the read loop. A full buffer
		// means the engine is behind; the book is now suspect, so drop the
		// frame, count it, and force a resync to rebuild from a fresh
		// snapshot rather than silently applying a gap.
		select {
		case out <- ev:
		default:
			metrics.DroppedEvents.Add(1)
			f.RequestResync()
		}
		return nil
	}
	return wsutil.RunForever(ctx, URL, subs, onMessage, wsutil.Options{
		Name:         "coinbase",
		ReadLimit:    readLimit,
		PingInterval: 15 * time.Second, // protocol ping; read loop is running
		Restart:      f.resync,
	})
}
