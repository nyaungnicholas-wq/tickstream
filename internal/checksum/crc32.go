// Package checksum implements the Kraken WebSocket v2 book CRC32 (spec §6.5,
// algorithm ✅ verified against Kraken's published vector = 3310070434).
package checksum

import (
	"hash/crc32"
	"strings"
)

// FmtVal formats one price or qty for the checksum: take the EXACT decimal
// string as delivered on the wire, remove the ".", strip ALL leading zeros,
// and do NOT re-pad.
//
//	"45285.2"    -> "452852"
//	"0.00100000" -> "000100000" -> "100000"
//
// This is why Level keeps the original wire strings: a float64 round-trip
// would not reproduce the delivered digits.
//
// NOTE: FmtVal only ever formats PRESENT levels (qty > 0). A qty of "0" is a
// delete, so the level is removed from the book and is never in the top-10 —
// FmtVal never receives "0" as a qty in the checksum path. (It is still
// covered defensively in tests; it is not a checksum-affecting decision.)
func FmtVal(decimalStr string) string {
	s := strings.ReplaceAll(decimalStr, ".", "")
	return strings.TrimLeft(s, "0")
}

// BookChecksum computes the Kraken book checksum over the top 10 levels of
// each side, AFTER all updates in the message have been applied.
//
// Each level is [2]string{priceStr, qtyStr}; each token is
// FmtVal(price)+FmtVal(qty). Concatenation order: all 10 ASK tokens first
// (price low→high), then all 10 BID tokens (price high→low), CRC32-IEEE over
// the UTF-8 bytes.
func BookChecksum(asksTop10, bidsTop10 [][2]string) uint32 {
	var b strings.Builder
	for _, lv := range asksTop10 {
		b.WriteString(FmtVal(lv[0]))
		b.WriteString(FmtVal(lv[1]))
	}
	for _, lv := range bidsTop10 {
		b.WriteString(FmtVal(lv[0]))
		b.WriteString(FmtVal(lv[1]))
	}
	return crc32.ChecksumIEEE([]byte(b.String()))
}
