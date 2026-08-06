// Package coinbase decodes Coinbase EXCHANGE `level2_batch` messages
// (spec §5.1; M0.5 gate-verified public/no-auth on 2026-07-01).
// It also decodes `match`/`last_match` trade frames.
//
// Schema notes (Exchange product — NOT Advanced Trade):
//   - "snapshot": bids/asks are [price, size] string tuples.
//   - "l2update": changes are [side, price, size] string tuples where side is
//     "buy"/"sell" and size is the NEW ABSOLUTE size ("0" = delete).
//   - "match"/"last_match": trade events where side is the MAKER side (not taker).
//   - No per-message sequence number on level2; TCP order + heartbeat
//     liveness (spec §6.3).
package coinbase

import (
	"fmt"
	"strconv"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/feed/jsonx"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

type envelope struct {
	Type      string     `json:"type"`
	ProductID string     `json:"product_id"`
	Time      string     `json:"time"`
	Bids      [][]string `json:"bids"`
	Asks      [][]string `json:"asks"`
	Changes   [][]string `json:"changes"`
	Side      string     `json:"side"`
	Size      string     `json:"size"`
	Price     string     `json:"price"`
	TradeID   int64      `json:"trade_id"`
}

// DecodeMessage decodes one raw frame. ok is false for frames we deliberately
// ignore (subscriptions ack, heartbeat, status); err is non-nil only for
// frames that should have decoded but didn't.
func DecodeMessage(data []byte) (model.Event, bool, error) {
	var env envelope
	if err := jsonx.Unmarshal(data, &env); err != nil {
		return model.Event{}, false, fmt.Errorf("coinbase: decode envelope: %w", err)
	}
	switch env.Type {
	case "snapshot":
		ev := model.Event{
			Venue:  model.Coinbase,
			Kind:   model.KindSnapshot,
			Symbol: env.ProductID,
		}
		var err error
		if ev.Bids, err = tupleLevels(env.Bids); err != nil {
			return model.Event{}, false, fmt.Errorf("coinbase snapshot bids: %w", err)
		}
		if ev.Asks, err = tupleLevels(env.Asks); err != nil {
			return model.Event{}, false, fmt.Errorf("coinbase snapshot asks: %w", err)
		}
		return ev, true, nil

	case "l2update":
		ev := model.Event{
			Venue:          model.Coinbase,
			Kind:           model.KindUpdate,
			Symbol:         env.ProductID,
			EventTimeNanos: parseTimeNanos(env.Time),
		}
		for _, ch := range env.Changes {
			if len(ch) != 3 {
				return model.Event{}, false, fmt.Errorf("coinbase l2update: change has %d fields, want 3", len(ch))
			}
			lv, err := level(ch[1], ch[2])
			if err != nil {
				return model.Event{}, false, fmt.Errorf("coinbase l2update: %w", err)
			}
			switch ch[0] {
			case "buy":
				ev.Bids = append(ev.Bids, lv)
			case "sell":
				ev.Asks = append(ev.Asks, lv)
			default:
				return model.Event{}, false, fmt.Errorf("coinbase l2update: unknown side %q", ch[0])
			}
		}
		return ev, true, nil

	case "match", "last_match":
		if env.Size == "" {
			return model.Event{}, false, fmt.Errorf("coinbase match: empty size")
		}
		if env.Price == "" {
			return model.Event{}, false, fmt.Errorf("coinbase match: empty price")
		}

		// Coinbase reports the side of the MAKER order (the resting order
		// that was hit), not the taker. Kraken v2 reports the taker side;
		// only Coinbase is inverted. The aggressor is therefore the
		// OPPOSITE side.
		var aggressor model.Side
		switch env.Side {
		case "sell":
			// A resting sell was lifted, so the taker bought.
			aggressor = model.Bid
		case "buy":
			// A resting buy was hit, so the taker sold.
			aggressor = model.Ask
		default:
			return model.Event{}, false, fmt.Errorf("coinbase match: unknown side %q", env.Side)
		}

		p, err := model.PriceFromString(env.Price)
		if err != nil {
			return model.Event{}, false, fmt.Errorf("coinbase match: %w", err)
		}
		q, err := model.QtyFromString(env.Size)
		if err != nil {
			return model.Event{}, false, fmt.Errorf("coinbase match: %w", err)
		}

		ev := model.Event{
			Venue:          model.Coinbase,
			Kind:           model.KindTrade,
			Symbol:         env.ProductID,
			EventTimeNanos: parseTimeNanos(env.Time),
			Trades: []model.Trade{
				{
					Price:     p,
					Qty:       q,
					PriceStr:  env.Price,
					QtyStr:    env.Size,
					Aggressor: aggressor,
					TimeNanos: parseTimeNanos(env.Time),
					ID:        strconv.FormatInt(env.TradeID, 10),
				},
			},
		}
		return ev, true, nil

	default:
		// "subscriptions", "heartbeat", "status", "error", ...: not book events.
		return model.Event{}, false, nil
	}
}

func tupleLevels(tuples [][]string) ([]model.Level, error) {
	out := make([]model.Level, 0, len(tuples))
	for _, t := range tuples {
		if len(t) != 2 {
			return nil, fmt.Errorf("level tuple has %d fields, want 2", len(t))
		}
		lv, err := level(t[0], t[1])
		if err != nil {
			return nil, err
		}
		out = append(out, lv)
	}
	return out, nil
}

func level(priceStr, qtyStr string) (model.Level, error) {
	p, err := model.PriceFromString(priceStr)
	if err != nil {
		return model.Level{}, err
	}
	q, err := model.QtyFromString(qtyStr)
	if err != nil {
		return model.Level{}, err
	}
	return model.Level{Price: p, Qty: q, PriceStr: priceStr, QtyStr: qtyStr}, nil
}

func parseTimeNanos(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0 // best-effort; event time is informational
	}
	return t.UnixNano()
}
