package types

import "github.com/arturoeanton/pg2json/internal/jsonwriter"

// EncodeJSONPassthrough drops json/jsonb's text representation directly
// into the output. PostgreSQL guarantees the text form is valid JSON
// (jsonb is canonicalised; json is validated at insert time). We trust
// that contract, which is the whole point of this fast path.
func EncodeJSONPassthrough(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) == 0 {
		// Empty body would produce invalid JSON; render as empty string
		// rather than corrupting the document.
		return append(dst, '"', '"')
	}
	return append(dst, raw...)
}
