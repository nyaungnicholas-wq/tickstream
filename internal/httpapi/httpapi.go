// Package httpapi serves the local dashboard: a single embedded HTML page and
// a JSON snapshot endpoint. It is a READER — every request is one wait-free
// Publisher.Load() plus value copies; it can never block or slow the engine.
package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/nyaungnicholas-wq/tickstream/internal/metrics"
	"github.com/nyaungnicholas-wq/tickstream/internal/model"
	"github.com/nyaungnicholas-wq/tickstream/internal/snapshot"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// levelDTO renders one price level. Prices/qtys travel as decimal strings —
// the UI must never do float math on money either.
type levelDTO struct {
	Price string `json:"price"`
	Qty   string `json:"qty"`
}

type venueDTO struct {
	Venue string     `json:"venue"`
	Ready bool       `json:"ready"`
	Bids  []levelDTO `json:"bids"`
	Asks  []levelDTO `json:"asks"`
}

type snapshotDTO struct {
	Valid       bool    `json:"valid"`
	BidPrice    string  `json:"bidPrice,omitempty"`
	BidSize     string  `json:"bidSize,omitempty"`
	BidVenue    string  `json:"bidVenue,omitempty"`
	AskPrice    string  `json:"askPrice,omitempty"`
	AskSize     string  `json:"askSize,omitempty"`
	AskVenue    string  `json:"askVenue,omitempty"`
	Spread      string  `json:"spread,omitempty"`
	Mid         float64 `json:"mid"`
	WeightedMid float64 `json:"weightedMid"`
	ImbSigned   float64 `json:"imbSigned"`
	ImbFraction float64 `json:"imbFraction"`
	Crossed     bool    `json:"crossed"`

	ApplyLatencyNanos int64      `json:"applyLatencyNanos"`
	PublishUnixNanos  int64      `json:"publishUnixNanos"`
	Venues            []venueDTO `json:"venues"`

	Metrics map[string]int64 `json:"metrics"`
}

func toLevels(levels []model.Level) []levelDTO {
	out := make([]levelDTO, 0, len(levels))
	for _, lv := range levels {
		out = append(out, levelDTO{Price: lv.Price.String(), Qty: lv.Qty.String()})
	}
	return out
}

func buildDTO(s *model.Snapshot) snapshotDTO {
	dto := snapshotDTO{
		Metrics: map[string]int64{
			"drops":              metrics.DroppedEvents.Load(),
			"resyncs":            metrics.Resyncs.Load(),
			"checksumMismatches": metrics.ChecksumMismatches.Load(),
			"thinSkips":          metrics.ChecksumSkippedThin.Load(),
			"reconnects":         metrics.Reconnects.Load(),
			"xvenueCrosses":      metrics.CrossVenueCrosses.Load(),
		},
	}
	if s == nil {
		return dto
	}
	dto.ApplyLatencyNanos = s.ApplyLatencyNanos
	dto.PublishUnixNanos = s.PublishUnixNanos
	for _, v := range []model.Venue{model.Coinbase, model.Kraken} {
		vt, ok := s.Venues[v]
		if !ok {
			continue
		}
		dto.Venues = append(dto.Venues, venueDTO{
			Venue: v.String(),
			Ready: vt.Ready,
			Bids:  toLevels(vt.Bids),
			Asks:  toLevels(vt.Asks),
		})
	}
	c := s.Consolidated
	if !c.Valid {
		return dto
	}
	dto.Valid = true
	dto.BidPrice, dto.BidSize, dto.BidVenue = c.BestBidPrice.String(), c.BestBidSize.String(), c.BestBidVenue.String()
	dto.AskPrice, dto.AskSize, dto.AskVenue = c.BestAskPrice.String(), c.BestAskSize.String(), c.BestAskVenue.String()
	dto.Spread = c.Spread.String()
	dto.Mid, dto.WeightedMid = s.Mid, s.WeightedMid
	dto.ImbSigned, dto.ImbFraction = s.ImbalanceSigned, s.ImbalanceFraction
	dto.Crossed = s.CrossVenueCrossed
	return dto
}

// Serve runs the dashboard server on addr until ctx is canceled.
func Serve(ctx context.Context, addr string, pub *snapshot.Publisher) error {
	mux := http.NewServeMux()

	page, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		return err
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(buildDTO(pub.Load())); err != nil {
			slog.Warn("dashboard: encode snapshot", "err", err)
		}
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	slog.Info("dashboard listening", "url", "http://"+addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
