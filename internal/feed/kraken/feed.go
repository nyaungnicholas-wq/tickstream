package kraken

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/feed/wsutil"
	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// URL is the Kraken websocket v2 endpoint (NOT the deprecated v1 URL).
const URL = "wss://ws.kraken.com/v2"

// Feed is the Kraken v2 book adapter.
type Feed struct {
	symbol string
	depth  int
	resync chan struct{}
}

// New returns a Kraken feed for one symbol (v2 slash format, e.g. "BTC/USD")
// at the given book depth (v1 uses 10).
func New(symbol string, depth int) *Feed {
	return &Feed{symbol: symbol, depth: depth, resync: make(chan struct{}, 1)}
}

// Venue implements feed.Feed.
func (f *Feed) Venue() model.Venue { return model.Kraken }

// RequestResync implements feed.Feed: non-blocking, coalescing signal.
func (f *Feed) RequestResync() {
	select {
	case f.resync <- struct{}{}:
	default:
	}
}

// Run implements feed.Feed. It reconnects forever until ctx is canceled.
// Kraken v2 book has no sequence numbers: every reconnect starts with a fresh
// snapshot (snapshot:true), which is exactly the resync procedure (§6.7).
func (f *Feed) Run(ctx context.Context, out chan<- model.Event) error {
	subs := [][]byte{
		[]byte(fmt.Sprintf(
			`{"method":"subscribe","params":{"channel":"book","symbol":[%q],"depth":%d,"snapshot":true},"req_id":1}`,
			f.symbol, f.depth,
		)),
	}
	onMessage := func(data []byte) error {
		received := time.Now() // apply-latency clock starts here (§9.1)
		ev, ok, err := DecodeMessage(data)
		if err != nil {
			slog.Warn("kraken: undecodable frame", "err", err)
			return nil
		}
		if !ok {
			return nil
		}
		ev.Received = received
		// Bounded hand-off (§4.4): never block the read loop; a dropped
		// frame makes the book suspect -> resync.
		select {
		case out <- ev:
		default:
			metrics.DroppedEvents.Add(1)
			f.RequestResync()
		}
		return nil
	}
	return wsutil.RunForever(ctx, URL, subs, onMessage, wsutil.Options{
		Name:      "kraken",
		ReadLimit: 1 << 20,
		// Kraken closes idle connections after ~1 min; send the documented
		// application-level ping.
		PingInterval: 15 * time.Second,
		AppPing:      []byte(`{"method":"ping","req_id":101}`),
		Restart:      f.resync,
	})
}
