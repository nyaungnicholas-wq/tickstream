// Package jsonx is the single seam for the JSON decoder choice.
//
// Primary: bytedance/sonic (JIT/SIMD on amd64/arm64; claimed ~5x stdlib
// Unmarshal — UNVERIFIED here, do not quote without a benchstat run).
// Fallback if sonic misbehaves on a platform/Go version: swap the import to
// github.com/goccy/go-json (drop-in signatures) — one-line change here only.
package jsonx

import (
	"github.com/bytedance/sonic"
)

// Unmarshal decodes JSON into v. sonic falls back to encoding/json semantics
// on unsupported architectures (correct, just not accelerated).
func Unmarshal(data []byte, v any) error {
	return sonic.Unmarshal(data, v)
}
