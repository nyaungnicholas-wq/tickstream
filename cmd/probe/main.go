// Command probe is the M0.5 connectivity GATE — a throwaway script whose only
// job is to prove that a PUBLIC, NO-AUTH Level-2 feed actually connects,
// before M3 is built on that assumption. It may be deleted after the decision
// is recorded in docs/architecture.md.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	cbPass, cbFirst := probeCoinbase()
	krPass, krFirst := probeKraken()

	fmt.Println("──────────────────────────────────────────")
	fmt.Printf("PROBE 1 Coinbase level2_batch : %s (first book msg type: %q)\n", passFail(cbPass), cbFirst)
	fmt.Printf("PROBE 2 Kraken v2 book        : %s (first book msg type: %q)\n", passFail(krPass), krFirst)
	switch {
	case cbPass && krPass:
		fmt.Println("DECISION: BOTH pass -> proceed to M3 as a TWO-VENUE build (Coinbase + Kraken).")
	case krPass:
		fmt.Println("DECISION: Coinbase FAILED -> fall back to KRAKEN-ONLY v1.")
	default:
		fmt.Println("DECISION: Kraken failed too -> STOP, do not start M3.")
		os.Exit(1)
	}
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// probeCoinbase dials Coinbase Exchange, subscribes level2_batch with no auth,
// and succeeds iff a "snapshot" message arrives within ~10s.
func probeCoinbase() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "wss://ws-feed.exchange.coinbase.com", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coinbase: dial: %v\n", err)
		return false, ""
	}
	defer c.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck // throwaway probe; best-effort close
	// The full BTC-USD level2 snapshot frame exceeds 1MB (measured at M0.5) —
	// a 1MB read limit fails with "message too big", NOT an auth error.
	c.SetReadLimit(64 << 20)

	sub := `{"type":"subscribe","product_ids":["BTC-USD"],"channels":["level2_batch"]}`
	if err := c.Write(ctx, websocket.MessageText, []byte(sub)); err != nil {
		fmt.Fprintf(os.Stderr, "coinbase: subscribe: %v\n", err)
		return false, ""
	}

	first := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithDeadline(ctx, deadline)
		_, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "coinbase: read: %v\n", err)
			return false, first
		}
		var env struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Bids    []any  `json:"bids"`
			Asks    []any  `json:"asks"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if first == "" && env.Type != "" {
			first = env.Type
		}
		switch env.Type {
		case "snapshot":
			fmt.Printf("coinbase: got snapshot with %d bids / %d asks — no auth needed\n", len(env.Bids), len(env.Asks))
			return true, "snapshot"
		case "error":
			fmt.Fprintf(os.Stderr, "coinbase: error frame: %s\n", string(data))
			return false, first
		}
	}
	return false, first
}

// probeKraken dials Kraken WS v2, subscribes the public book channel, and
// succeeds iff a book snapshot arrives within ~10s.
func probeKraken() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "wss://ws.kraken.com/v2", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kraken: dial: %v\n", err)
		return false, ""
	}
	defer c.Close(websocket.StatusNormalClosure, "done") //nolint:errcheck // throwaway probe; best-effort close
	c.SetReadLimit(1 << 20)

	sub := `{"method":"subscribe","params":{"channel":"book","symbol":["BTC/USD"],"depth":10,"snapshot":true},"req_id":1}`
	if err := c.Write(ctx, websocket.MessageText, []byte(sub)); err != nil {
		fmt.Fprintf(os.Stderr, "kraken: subscribe: %v\n", err)
		return false, ""
	}

	first := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithDeadline(ctx, deadline)
		_, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "kraken: read: %v\n", err)
			return false, first
		}
		var env struct {
			Channel string `json:"channel"`
			Type    string `json:"type"`
			Method  string `json:"method"`
			Success *bool  `json:"success"`
			Data    []struct {
				Bids     []any  `json:"bids"`
				Asks     []any  `json:"asks"`
				Checksum uint32 `json:"checksum"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Channel == "book" {
			if first == "" {
				first = env.Type
			}
			if env.Type == "snapshot" && len(env.Data) > 0 {
				fmt.Printf("kraken: got book snapshot with %d bids / %d asks, checksum=%d — no auth needed\n",
					len(env.Data[0].Bids), len(env.Data[0].Asks), env.Data[0].Checksum)
				return true, "snapshot"
			}
		}
		if env.Method == "subscribe" && env.Success != nil && !*env.Success {
			fmt.Fprintf(os.Stderr, "kraken: subscribe rejected: %s\n", string(data))
			return false, first
		}
	}
	return false, first
}
