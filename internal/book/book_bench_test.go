package book

import (
	"fmt"
	"testing"

	"github.com/nyaungnicholas-wq/tickstream/internal/model"
)

// benchBook builds a ready book with n levels per side around 50_000.00.
func benchBook(b *testing.B, n int) *Book {
	b.Helper()
	bids := make([]model.Level, 0, n)
	asks := make([]model.Level, 0, n)
	for i := 0; i < n; i++ {
		bidStr := fmt.Sprintf("%d.5", 50_000-1-i)
		askStr := fmt.Sprintf("%d.5", 50_000+1+i)
		bids = append(bids, mustLevel(b, bidStr, "1.5"))
		asks = append(asks, mustLevel(b, askStr, "2.5"))
	}
	bk := New()
	bk.ApplySnapshot(bids, asks)
	return bk
}

func mustLevel(b *testing.B, price, qty string) model.Level {
	b.Helper()
	p, err := model.PriceFromString(price)
	if err != nil {
		b.Fatal(err)
	}
	q, err := model.QtyFromString(qty)
	if err != nil {
		b.Fatal(err)
	}
	return model.Level{Price: p, Qty: q, PriceStr: price, QtyStr: qty}
}

// BenchmarkApply measures the common case: an absolute-size overwrite of an
// existing level near the top of a full-depth book (map write, no memmove).
func BenchmarkApply(b *testing.B) {
	bk := benchBook(b, 10_000)
	upd := []model.Level{mustLevel(b, "49997.5", "3.14159265")}
	b.ReportAllocs()
	for b.Loop() {
		bk.ApplyUpdate(upd, nil)
	}
}

// BenchmarkApplyDeepInsert measures the worst case this structure admits:
// alternately inserting and deleting the DEEPEST level, which memmoves the
// whole price slice each time (the honest O(P) path, package comment).
func BenchmarkApplyDeepInsert(b *testing.B) {
	bk := benchBook(b, 10_000)
	ins := []model.Level{mustLevel(b, "1.5", "1.0")}
	del := []model.Level{mustLevel(b, "1.5", "0")}
	b.ReportAllocs()
	for b.Loop() {
		bk.ApplyUpdate(ins, nil)
		bk.ApplyUpdate(del, nil)
	}
}

// BenchmarkBestBid measures the O(1) top-of-book read.
func BenchmarkBestBid(b *testing.B) {
	bk := benchBook(b, 10_000)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := bk.BestBid(); !ok {
			b.Fatal("empty")
		}
	}
}

// BenchmarkDeleteBest measures the delete-of-current-best hot path (paired
// with a reinsert so the book stays full). With the best-at-end layout the
// delete is a pop; the reinsert appends at the end — no memmove either way.
func BenchmarkDeleteBest(b *testing.B) {
	bk := benchBook(b, 10_000)
	del := []model.Level{mustLevel(b, "49999.5", "0")}
	ins := []model.Level{mustLevel(b, "49999.5", "1.5")}
	b.ReportAllocs()
	for b.Loop() {
		bk.ApplyUpdate(del, nil)
		bk.ApplyUpdate(ins, nil)
	}
}

// BenchmarkTopN measures building the fresh top-10 slice used per publish.
func BenchmarkTopN(b *testing.B) {
	bk := benchBook(b, 10_000)
	b.ReportAllocs()
	for b.Loop() {
		_ = bk.TopN(model.Bid, 10)
	}
}
