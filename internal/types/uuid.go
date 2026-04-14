package types

import "github.com/arturoeanton/pg2json/internal/jsonwriter"

// EncodeUUID writes a quoted UUID. UUID text form is exactly 36 ASCII
// characters (8-4-4-4-12 hex with dashes). We do a length check for safety
// but skip per-byte validation: if the server gave us 36 ASCII chars
// labelled "uuid" we trust them.
func EncodeUUID(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 36 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	dst = append(dst, '"')
	dst = append(dst, raw...)
	return append(dst, '"')
}
