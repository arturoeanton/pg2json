package types

import (
	"encoding/binary"
	"strconv"

	"github.com/arturoeanton/pg2json/internal/jsonwriter"
)

// EncodeNumericBinary is the exported alias for encodeNumericBinary.
// The default PickBinary dispatch does NOT pick it — on loopback the
// text path is faster for typical precisions. Callers that want binary
// numeric (wide precision on a high-RTT link) can build a custom
// Encoder table referencing this symbol directly.
var EncodeNumericBinary = encodeNumericBinary

// encodeNumericBinary decodes PG numeric wire format and emits a canonical
// decimal string. Wire layout (see src/backend/utils/adt/numeric.c):
//
//	int16 ndigits   — number of base-10000 digits that follow
//	int16 weight    — weight of the first digit in base-10000
//	uint16 sign     — 0x0000 pos, 0x4000 neg, 0xC000 NaN, 0xD000 +Inf, 0xF000 -Inf
//	int16 dscale    — display scale, i.e. # of decimal digits after the point
//	int16[ndigits] digits — each in [0, 9999]
//
// Value = sign × Σ digit[i] × 10000^(weight - i). Special signs render as
// JSON strings (they are not valid JSON numbers). For the numeric-heavy
// workload this avoids the single-pass text validation EncodeNumeric does
// and — more importantly — gets us out of the server's base-10 text
// rendering, which is measurably more expensive than the base-10000
// binary path for wide values.
func encodeNumericBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) < 8 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	ndigits := int(int16(binary.BigEndian.Uint16(raw[0:2])))
	weight := int(int16(binary.BigEndian.Uint16(raw[2:4])))
	sign := binary.BigEndian.Uint16(raw[4:6])
	dscale := int(int16(binary.BigEndian.Uint16(raw[6:8])))
	body := raw[8:]
	if ndigits < 0 || dscale < 0 || len(body) < ndigits*2 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}

	switch sign {
	case 0x0000, 0x4000:
		// plain positive / negative
	case 0xC000:
		return append(dst, '"', 'N', 'a', 'N', '"')
	case 0xD000:
		return append(dst, '"', 'I', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	case 0xF000:
		return append(dst, '"', '-', 'I', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	default:
		return jsonwriter.AppendStringBytes(dst, raw)
	}

	negative := sign == 0x4000

	digitAt := func(i int) int {
		if i < 0 || i >= ndigits {
			return 0
		}
		return int(binary.BigEndian.Uint16(body[i*2:]))
	}

	// Zero: ndigits == 0 (Postgres canonicalises -0 to sign == 0).
	if ndigits == 0 {
		dst = append(dst, '0')
		if dscale > 0 {
			dst = append(dst, '.')
			for i := 0; i < dscale; i++ {
				dst = append(dst, '0')
			}
		}
		return dst
	}

	if negative {
		dst = append(dst, '-')
	}

	// Integer portion. weight >= 0 means the first base-10000 digit is
	// at least the units place; lower-weight digits are fractional. If
	// weight exceeds ndigits-1, the missing integer digits are zero.
	if weight < 0 {
		dst = append(dst, '0')
	} else {
		dst = strconv.AppendInt(dst, int64(digitAt(0)), 10)
		for i := 1; i <= weight; i++ {
			dst = appendNumericPad4(dst, digitAt(i))
		}
	}

	if dscale == 0 {
		return dst
	}
	dst = append(dst, '.')
	// Each base-10000 digit carries 4 decimal digits. For output decimal
	// position p (0-indexed from the point), the source base-10000 digit
	// is at index (weight + 1 + p/4) and the decimal position within that
	// base-10000 digit is (p % 4). divisors pulls out thousands / hundreds
	// / tens / units.
	var divisors = [4]int{1000, 100, 10, 1}
	for p := 0; p < dscale; p++ {
		idx := weight + 1 + p/4
		inner := p & 3
		digit := (digitAt(idx) / divisors[inner]) % 10
		dst = append(dst, byte('0'+digit))
	}
	return dst
}

func appendNumericPad4(dst []byte, v int) []byte {
	return append(dst,
		byte('0'+v/1000%10),
		byte('0'+v/100%10),
		byte('0'+v/10%10),
		byte('0'+v%10))
}
