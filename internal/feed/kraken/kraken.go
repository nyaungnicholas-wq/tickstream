// Package kraken decodes Kraken WebSocket v2 public messages: book (spec §5.2)
// and trade (spec §5.3). M0.5 gate-verified public/no-auth on 2026-07-01.
//
// Schema notes:
//   - price/qty arrive as JSON NUMBERS. The Kraken checksum is computed over
//     the EXACT delivered decimal digits, so we decode them as raw tokens and
//     preserve the verbatim strings in Level.PriceStr/QtyStr — never through
//     float64.
//   - qty == 0 => delete. Every snapshot/update carries `checksum` over the
//     top-10 of each side after applying the message.
//   - No sequence numbers: on checksum mismatch or disconnect, resync.
//   - Trade messages batch multiple prints per frame; dropping any would lose
//     volume, so we emit all trades in the slice.
//   - Kraken's trade.side is the taker's (aggressor's) side, so it maps
//     directly with no inversion. This differs from Coinbase Exchange, which
//     reports the maker side — the asymmetry is the trap.
package kraken

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/feed/jsonx"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

type envelope struct {
	Channel string          `json:"channel"`
	Type    string          `json:"type"`
	Method  string          `json:"method"`
	Data    json.RawMessage `json:"data"`
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

type tradeData struct {
	Symbol    string          `json:"symbol"`
	Side      string          `json:"side"`
	Price     json.RawMessage `json:"price"`
	Qty       json.RawMessage `json:"qty"`
	TradeID   int64           `json:"trade_id"`
	Timestamp string          `json:"timestamp"`
}

// DecodeMessage decodes one raw frame. ok is false for frames we deliberately
// ignore (subscribe acks, heartbeat, status, pong).
func DecodeMessage(data []byte) (model.Event, bool, error) {
	var env envelope
	if err := jsonx.Unmarshal(data, &env); err != nil {
		return model.Event{}, false, fmt.Errorf("kraken: decode envelope: %w", err)
	}

	switch env.Channel {
	case "book":
		return decodeBook(env)
	case "trade":
		return decodeTrade(env)
	default:
		// heartbeat/status channels, method acks (subscribe/pong), etc.
		return model.Event{}, false, nil
	}
}

func decodeBook(env envelope) (model.Event, bool, error) {
	var kind model.EventKind
	switch env.Type {
	case "snapshot":
		kind = model.KindSnapshot
	case "update":
		kind = model.KindUpdate
	default:
		return model.Event{}, false, nil
	}
	var data []bookData
	if err := jsonx.Unmarshal(env.Data, &data); err != nil {
		return model.Event{}, false, fmt.Errorf("kraken: decode book data: %w", err)
	}
	if len(data) == 0 {
		return model.Event{}, false, fmt.Errorf("kraken: %s frame with empty data", env.Type)
	}
	d := data[0]
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

func decodeTrade(env envelope) (model.Event, bool, error) {
	switch env.Type {
	case "snapshot", "update":
		// proceed
	default:
		return model.Event{}, false, nil
	}
	var data []tradeData
	if err := jsonx.Unmarshal(env.Data, &data); err != nil {
		return model.Event{}, false, fmt.Errorf("kraken: decode trade data: %w", err)
	}
	if len(data) == 0 {
		return model.Event{}, false, fmt.Errorf("kraken: trade frame with empty data")
	}
	// Batched trades; all share the same symbol and timestamp.
	d := data[0]
	ev := model.Event{
		Venue:          model.Kraken,
		Kind:           model.KindTrade,
		Symbol:         d.Symbol,
		EventTimeNanos: parseTimeNanos(d.Timestamp),
		Trades:         make([]model.Trade, 0, len(data)),
	}
	for _, t := range data {
		priceStr := string(bytes.TrimSpace(t.Price))
		qtyStr := string(bytes.TrimSpace(t.Qty))
		price, err := model.PriceFromString(priceStr)
		if err != nil {
			return model.Event{}, false, fmt.Errorf("kraken trade price: %w", err)
		}
		qty, err := model.QtyFromString(qtyStr)
		if err != nil {
			return model.Event{}, false, fmt.Errorf("kraken trade qty: %w", err)
		}
		var aggressor model.Side
		switch t.Side {
		case "buy":
			aggressor = model.Bid
		case "sell":
			aggressor = model.Ask
		default:
			return model.Event{}, false, fmt.Errorf("kraken trade: unknown side %q", t.Side)
		}
		ev.Trades = append(ev.Trades, model.Trade{
			Price:     price,
			Qty:       qty,
			PriceStr:  priceStr,
			QtyStr:    qtyStr,
			Aggressor: aggressor,
			TimeNanos: parseTimeNanos(t.Timestamp),
			ID:        strconv.FormatInt(t.TradeID, 10),
		})
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
