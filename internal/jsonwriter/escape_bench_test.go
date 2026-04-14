package jsonwriter

import (
	"strings"
	"testing"
)

// Benchmarks to compare the scalar vs SWAR escape paths. Run with:
//
//	go test -bench=BenchmarkAppendStringBody -benchmem ./internal/jsonwriter
//	go test -tags pg2json_simd -bench=BenchmarkAppendStringBody -benchmem ./internal/jsonwriter

var (
	// Short ASCII: typical identifier / enum value.
	shortASCII = "active-user-42"
	// 128-byte ASCII: typical description / text column.
	mediumASCII = strings.Repeat("abcdefghijklmnop", 8)
	// 1 KiB ASCII: large text / jsonb-text column.
	longASCII = strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789 ", 28)[:1024]
	// 128 bytes with one quote at the middle.
	withQuote = strings.Repeat("abcdefgh", 8) + `"` + strings.Repeat("ijklmnop", 7)
	// Worst case: every 4th byte needs escaping.
	heavyEscape = strings.Repeat("abc\"", 64)
)

func BenchmarkAppendStringBody_ShortASCII(b *testing.B) {
	dst := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendStringBody(dst[:0], shortASCII)
	}
	_ = dst
}

func BenchmarkAppendStringBody_MediumASCII(b *testing.B) {
	dst := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendStringBody(dst[:0], mediumASCII)
	}
	_ = dst
}

func BenchmarkAppendStringBody_LongASCII(b *testing.B) {
	dst := make([]byte, 0, 2048)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendStringBody(dst[:0], longASCII)
	}
	_ = dst
}

func BenchmarkAppendStringBody_WithQuote(b *testing.B) {
	dst := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendStringBody(dst[:0], withQuote)
	}
	_ = dst
}

func BenchmarkAppendStringBody_HeavyEscape(b *testing.B) {
	dst := make([]byte, 0, 512)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendStringBody(dst[:0], heavyEscape)
	}
	_ = dst
}
