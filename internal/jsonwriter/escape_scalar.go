//go:build !pg2json_simd

package jsonwriter

// AppendStringBody escapes s and appends it without surrounding quotes.
// This is the default scalar implementation. An alternative SWAR-based
// implementation is available under the pg2json_simd build tag
// (experimental; see escape_swar.go).
func AppendStringBody(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !escapeFlag[c] {
			continue
		}
		if i > start {
			dst = append(dst, s[start:i]...)
		}
		dst = appendEscape(dst, c)
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return dst
}

// AppendStringBodyBytes is the []byte twin of AppendStringBody.
func AppendStringBodyBytes(dst, s []byte) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !escapeFlag[c] {
			continue
		}
		if i > start {
			dst = append(dst, s[start:i]...)
		}
		dst = appendEscape(dst, c)
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return dst
}
