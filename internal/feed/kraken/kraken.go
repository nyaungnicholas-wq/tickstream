// Package kraken decodes Kraken WebSocket v2 `book` messages (spec §5.2;
// M0.5 gate-verified public/no-auth on 2026-07-01).
//
// Schema notes:
//   - price/qty arrive as JSON NUMBERS. The Kraken checksum is computed over
//     the EXACT delivered decimal digits, so we decode them as raw tokens and
//     preserve the verbatim strings in Level.PriceStr/QtyStr — never through
//     float64.
//   - qty == 0 => delete. Every snapshot/update carries `checksum` over the
//     top-10 of each side after applying the message.
//   - No sequence numbers: on checksum mismatch or disconnect, resync.
package kraken

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/feed/jsonx"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

type envelope struct {
	Channel string     `json:"channel"`
	Type    string     `json:"type"`
	Method  string     `json:"method"`
	Data    []bookData `json:"data"`
}

type bookData struct {
	Symbol    string     `json:"symbol"`
	Bids      []rawLevel `json:"bids"`
	Asks      []rawLevel `json:"asks"`
	Checksum  uint32     `json:"checksum"`
	Timestamp string     `json:"timestamp"`
}

// rawLevel captures price/qty as raw JSON tokens so the exact delivered
// decimal digits survive (checksum requirement, spec §6.5).
type rawLevel struct {
	Price json.RawMessage `json:"price"`
	Qty   json.RawMessage `json:"qty"`
}

// DecodeMessage decodes one raw frame. ok is false for frames we deliberately
// ignore (subscribe acks, heartbeat, status, pong).
func DecodeMessage(data []byte) (model.Event, bool, error) {
	var env envelope
	if err := jsonx.Unmarshal(data, &env); err != nil {
		return model.Event{}, false, fmt.Errorf("kraken: decode envelope: %w", err)
	}
	if env.Channel != "book" {
		// heartbeat/status channels, method acks (subscribe/pong), etc.
		return model.Event{}, false, nil
	}
	var kind model.EventKind
	switch env.Type {
	case "snapshot":
		kind = model.KindSnapshot
	case "update":
		kind = model.KindUpdate
	default:
		return model.Event{}, false, nil
	}
	if len(env.Data) == 0 {
		return model.Event{}, false, fmt.Errorf("kraken: %s frame with empty data", env.Type)
	}
	d := env.Data[0]
	ev := model.Event{
		Venue:          model.Kraken,
		Kind:           kind,
		Symbol:         d.Symbol,
		Checksum:       d.Checksum,
		EventTimeNanos: parseTimeNanos(d.Timestamp),
	}
	var err error
	if ev.Bids, err = levels(d.Bids); err != nil {
		return model.Event{}, false, fmt.Errorf("kraken %s bids: %w", env.Type, err)
	}
	if ev.Asks, err = levels(d.Asks); err != nil {
		return model.Event{}, false, fmt.Errorf("kraken %s asks: %w", env.Type, err)
	}
	return ev, true, nil
}

func levels(raw []rawLevel) ([]model.Level, error) {
	out := make([]model.Level, 0, len(raw))
	for _, r := range raw {
		priceStr := string(bytes.TrimSpace(r.Price))
		qtyStr := string(bytes.TrimSpace(r.Qty))
		p, err := model.PriceFromString(priceStr)
		if err != nil {
			return nil, err
		}
		q, err := model.QtyFromString(qtyStr)
		if err != nil {
			return nil, err
		}
		out = append(out, model.Level{Price: p, Qty: q, PriceStr: priceStr, QtyStr: qtyStr})
	}
	return out, nil
}

func parseTimeNanos(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixNano()
}
