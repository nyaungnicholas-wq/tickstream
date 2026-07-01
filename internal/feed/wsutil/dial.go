// Package wsutil provides the shared websocket plumbing: dial + subscribe +
// ping keepalive + read loop, wrapped in never-give-up reconnect backoff.
//
// THE DEADLOCK TRAP THIS PACKAGE IS DESIGNED AROUND (spec §4.4):
// coder/websocket's Ping(ctx) needs the READ LOOP running to receive the
// pong. If onMessage ever blocks (e.g. on a full engine channel), the read
// loop blocks → pings starve → the connection is declared dead → resync →
// which needs the engine → death spiral. Therefore onMessage MUST NOT block:
// the feed adapters hand events to the engine over a BOUNDED channel with a
// non-blocking send, and a full buffer is a DEFINED failure mode
// (drop + metric + resync), never a block.
package wsutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"

	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
)

// Options configures one websocket session.
type Options struct {
	// ReadLimit is the max inbound frame size in bytes. Coinbase full-book
	// snapshots exceed 1 MB (measured at the M0.5 gate), so the Coinbase
	// adapter must set this large; <= 0 keeps the library default (32 KiB).
	ReadLimit int64
	// PingInterval enables a keepalive ticker; 0 disables it.
	PingInterval time.Duration
	// AppPing, if set, is sent as a text frame instead of a protocol ping
	// (Kraken v2 wants an application-level {"method":"ping"}).
	AppPing []byte
	// Restart, when signaled, drops the current session so RunForever
	// redials (the engine uses this to force a venue resync).
	Restart <-chan struct{}
	// OnConnect runs after a successful dial + subscribe.
	OnConnect func()
	// Name labels log lines.
	Name string
}

var errRestart = errors.New("wsutil: restart requested (resync)")

// DialAndRun dials url, sends each subscribe message, starts the keepalive
// goroutine, and runs the read loop, calling onMessage for every frame.
// It returns when the session ends (error, ctx cancel, or restart signal).
// onMessage MUST NOT block (see the package comment).
func DialAndRun(ctx context.Context, url string, subs [][]byte, onMessage func([]byte) error, opts Options) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	defer c.CloseNow() //nolint:errcheck // session teardown is best-effort

	if opts.ReadLimit > 0 {
		c.SetReadLimit(opts.ReadLimit)
	}
	for _, s := range subs {
		if err := c.Write(ctx, websocket.MessageText, s); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}
	if opts.OnConnect != nil {
		opts.OnConnect()
	}

	sessCtx, sessCancel := context.WithCancelCause(ctx)
	defer sessCancel(nil)

	if opts.PingInterval > 0 {
		go pingLoop(sessCtx, sessCancel, c, opts)
	}
	if opts.Restart != nil {
		go func() {
			select {
			case <-sessCtx.Done():
			case <-opts.Restart:
				sessCancel(errRestart)
			}
		}()
	}

	// Read loop: drain frames promptly; onMessage never blocks.
	for {
		_, data, err := c.Read(sessCtx)
		if err != nil {
			if cause := context.Cause(sessCtx); errors.Is(cause, errRestart) {
				return errRestart
			}
			return fmt.Errorf("read: %w", err)
		}
		if err := onMessage(data); err != nil {
			return fmt.Errorf("onMessage: %w", err)
		}
	}
}

func pingLoop(ctx context.Context, cancel context.CancelCauseFunc, c *websocket.Conn, opts Options) {
	t := time.NewTicker(opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var err error
			if len(opts.AppPing) > 0 {
				err = c.Write(ctx, websocket.MessageText, opts.AppPing)
			} else {
				// Protocol ping: the concurrently-running read loop
				// receives the pong.
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				err = c.Ping(pingCtx)
				pingCancel()
			}
			if err != nil {
				cancel(fmt.Errorf("ping: %w", err))
				return
			}
		}
	}
}

// RunForever wraps DialAndRun in exponential backoff and never gives up until
// ctx is canceled. Backoff resets on every successful connect.
func RunForever(ctx context.Context, url string, subs [][]byte, onMessage func([]byte) error, opts Options) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 500 * time.Millisecond
	b.MaxInterval = 30 * time.Second
	b.MaxElapsedTime = 0 // retry forever

	userOnConnect := opts.OnConnect
	opts.OnConnect = func() {
		b.Reset()
		metrics.Reconnects.Add(1)
		if userOnConnect != nil {
			userOnConnect()
		}
	}

	for {
		err := DialAndRun(ctx, url, subs, onMessage, opts)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRestart) {
			slog.Info("session restart (resync requested)", "feed", opts.Name)
		} else {
			slog.Warn("session ended; will reconnect", "feed", opts.Name, "err", err)
		}
		wait := b.NextBackOff()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
