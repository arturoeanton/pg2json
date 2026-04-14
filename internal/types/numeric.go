package types

import "github.com/arturoeanton/pg2json/internal/jsonwriter"

// EncodeBool: text protocol returns "t" or "f".
func EncodeBool(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) == 1 && raw[0] == 't' {
		return jsonwriter.AppendBool(dst, true)
	}
	return jsonwriter.AppendBool(dst, false)
}

// EncodeInt validates and emits an integer as a raw JSON number. The
// validation is cheap (a single pass) and lets us drop the bytes in
// without strconv.
func EncodeInt(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if !validInt(raw) {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	return append(dst, raw...)
}

func validInt(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	i := 0
	if b[0] == '-' || b[0] == '+' {
		i++
		if i == len(b) {
			return false
		}
	}
	for ; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// EncodeFloat handles float4/float8. Postgres can emit "NaN", "Infinity",
// "-Infinity" — none of which are valid JSON numbers, so we string-quote
// them. Everything else is a valid JSON number and passes through.
func EncodeFloat(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if isNonFiniteFloat(raw) {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	if !validFloat(raw) {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	return append(dst, raw...)
}

// EncodeNumeric: same shape as float, but Postgres' numeric never uses
// exponential notation by default and "NaN" is the only special form.
func EncodeNumeric(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) == 3 && raw[0] == 'N' && raw[1] == 'a' && raw[2] == 'N' {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	if !validFloat(raw) {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	return append(dst, raw...)
}

func isNonFiniteFloat(b []byte) bool {
	switch len(b) {
	case 3:
		return b[0] == 'N' && b[1] == 'a' && b[2] == 'N'
	case 8:
		return b[0] == 'I' || (b[0] == '-' && b[1] == 'I')
	case 9:
		return b[0] == '-' && b[1] == 'I'
	}
	return false
}

// validFloat is permissive: digits, optional sign, one '.', one 'e'/'E'
// with optional sign. We do not need to reject every malformed string
// because the server is the source — we just need to be sure no embedded
// quote or letter slips through that would make our output invalid JSON.
func validFloat(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	i := 0
	if b[0] == '-' || b[0] == '+' {
		i++
	}
	sawDigit := false
	sawDot := false
	for ; i < len(b); i++ {
		c := b[i]
		if c >= '0' && c <= '9' {
			sawDigit = true
			continue
		}
		if c == '.' && !sawDot {
			sawDot = true
			continue
		}
		if (c == 'e' || c == 'E') && sawDigit && i+1 < len(b) {
			i++
			if b[i] == '+' || b[i] == '-' {
				i++
			}
			if i >= len(b) {
				return false
			}
			for ; i < len(b); i++ {
				if b[i] < '0' || b[i] > '9' {
					return false
				}
			}
			return true
		}
		return false
	}
	return sawDigit
}
