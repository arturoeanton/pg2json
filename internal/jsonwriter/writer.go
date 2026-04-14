// Package jsonwriter provides Append* helpers that write JSON directly into
// a caller-supplied []byte. There is no writer struct on purpose: passing
// the buffer back and forth as a []byte keeps the slice header in
// registers and matches strconv.Append* idioms.
package jsonwriter

import "unicode/utf8"

// AppendNull writes the JSON literal null.
func AppendNull(dst []byte) []byte { return append(dst, 'n', 'u', 'l', 'l') }

// AppendBool writes true or false.
func AppendBool(dst []byte, v bool) []byte {
	if v {
		return append(dst, 't', 'r', 'u', 'e')
	}
	return append(dst, 'f', 'a', 'l', 's', 'e')
}

// escapeFlag[b] is true when byte b must be escaped inside a JSON string.
// We escape: 0x00..0x1F, '"', '\\'. Bytes >= 0x80 are passed through
// (they are part of valid UTF-8 sequences; if the input is not UTF-8 the
// caller is responsible for sanitising).
var escapeFlag = func() [256]bool {
	var t [256]bool
	for i := 0; i < 0x20; i++ {
		t[i] = true
	}
	t['"'] = true
	t['\\'] = true
	return t
}()

// AppendString writes a JSON string. Quotes are added.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = AppendStringBody(dst, s)
	return append(dst, '"')
}

// AppendStringBytes is the []byte twin of AppendString. Avoids the string
// header allocation when the source is already a byte slice (e.g. straight
// from the wire).
func AppendStringBytes(dst, s []byte) []byte {
	dst = append(dst, '"')
	dst = AppendStringBodyBytes(dst, s)
	return append(dst, '"')
}

func appendEscape(dst []byte, c byte) []byte {
	switch c {
	case '"':
		return append(dst, '\\', '"')
	case '\\':
		return append(dst, '\\', '\\')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	default:
		// Other control char: \u00XX
		const hex = "0123456789abcdef"
		return append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
	}
}

// AppendKey writes a quoted JSON key followed by a colon. Used for
// pre-building per-column key prefixes during plan compilation.
func AppendKey(dst []byte, k string) []byte {
	dst = AppendString(dst, k)
	return append(dst, ':')
}

// ValidUTF8 reports whether p is valid UTF-8. We only call this in defensive
// paths; the hot loop trusts the server's encoding (typically UTF-8 since
// almost every Postgres database is initdb'd with UTF8).
func ValidUTF8(p []byte) bool { return utf8.Valid(p) }
