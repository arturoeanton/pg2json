//go:build pg2json_simd

// SWAR (SIMD Within A Register) implementation of AppendStringBody.
// Enabled with: go build -tags pg2json_simd
//
// EXPERIMENTAL. Not the default. The scalar implementation in
// escape_scalar.go is the correctness reference. This file must be
// semantically identical for every byte sequence — the fuzz suite
// guarantees that.
//
// Technique: check 8 bytes at a time using a uint64 load and bitwise
// tricks to detect any byte that requires escaping (< 0x20, '"' 0x22,
// '\' 0x5C). When an 8-byte chunk is entirely safe, the scan advances
// 8 bytes with no per-byte branches. When a chunk contains an
// escapable byte, we fall back to scalar scanning for that chunk only
// and then resume SWAR.
//
// Not true SIMD (no AVX2 / NEON). A real assembly implementation would
// be ~2-3× faster still, at the cost of per-arch .s files. This SWAR
// version is a pure-Go stepping stone with zero platform specifics.

package jsonwriter

import "encoding/binary"

const (
	swarOnes uint64 = 0x0101010101010101
	swarHigh uint64 = 0x8080808080808080
	swarQuote uint64 = 0x2222222222222222
	swarSlash uint64 = 0x5C5C5C5C5C5C5C5C
	swarCtrl  uint64 = 0x2020202020202020
)

// swarHasEscapable returns non-zero if any byte of x requires escaping
// (< 0x20, == 0x22, or == 0x5C). No branches.
//
// Per-byte primitives:
//   hasZero(y)    = (y - ones) & ^y & high  — non-zero in high bit of any zero byte.
//   hasLess(y, n) = (y - ones*n) & ^y & high  — non-zero in high bit of any byte < n,
//                   provided n < 0x80 so subtraction doesn't underflow across bytes.
func swarHasEscapable(x uint64) uint64 {
	lowCtrl := (x - swarCtrl) & ^x & swarHigh             // any byte < 0x20
	quote := ((x ^ swarQuote) - swarOnes) & ^(x ^ swarQuote) & swarHigh
	slash := ((x ^ swarSlash) - swarOnes) & ^(x ^ swarSlash) & swarHigh
	return lowCtrl | quote | slash
}

// AppendStringBody escapes s and appends it without surrounding quotes.
// SWAR variant: processes up to 8 safe bytes per iteration.
func AppendStringBody(dst []byte, s string) []byte {
	return appendStringBodySWAR(dst, []byte(s))
}

// AppendStringBodyBytes is the []byte twin of AppendStringBody.
func AppendStringBodyBytes(dst, s []byte) []byte {
	return appendStringBodySWAR(dst, s)
}

func appendStringBodySWAR(dst, s []byte) []byte {
	start := 0
	i := 0
	for i < len(s) {
		// Fast-forward through safe 8-byte chunks.
		for i+8 <= len(s) {
			chunk := binary.LittleEndian.Uint64(s[i:])
			if swarHasEscapable(chunk) != 0 {
				break
			}
			i += 8
		}
		// Scalar scan until we hit an escapable byte or end of input.
		for ; i < len(s); i++ {
			c := s[i]
			if escapeFlag[c] {
				break
			}
		}
		// Emit the accumulated safe span [start, i).
		if i > start {
			dst = append(dst, s[start:i]...)
			start = i
		}
		// Emit the escape for s[i] if we stopped on one.
		if i < len(s) {
			dst = appendEscape(dst, s[i])
			i++
			start = i
		}
	}
	return dst
}
