package types

import "github.com/arturoeanton/pg2json/internal/jsonwriter"

// EncodeBytea handles both bytea_output forms:
//   - "hex" (default since 9.0):   \xDEADBEEF
//   - "escape" (legacy):           octal-escaped ASCII
//
// In hex form, the only character that needs JSON escaping is the
// leading backslash; we write `"\\x...."` directly. In escape form we
// fall through to the generic string escaper.
func EncodeBytea(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) >= 2 && raw[0] == '\\' && raw[1] == 'x' {
		dst = append(dst, '"', '\\', '\\', 'x')
		dst = append(dst, raw[2:]...)
		return append(dst, '"')
	}
	return jsonwriter.AppendStringBytes(dst, raw)
}
