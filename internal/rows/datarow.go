package rows

import (
	"fmt"

	"github.com/arturoeanton/pg2json/internal/jsonwriter"
)

// DataRow body layout:
//   int16  column count
//   for each column:
//     int32  length (-1 for NULL)
//     bytes  value (length bytes; absent if length == -1)
//
// AppendObject decodes one DataRow body and appends a JSON object using
// the supplied plan. Returns the new dst.
//
// The column read loop is hand-inlined: a helper would add one function
// frame per cell and force the slice header through memory on every call.
// The hot loop runs per DataRow and this is the single biggest cell-level
// win we can bank without SIMD. The only shared helper is the bounds
// guard; keep the decoding arithmetic here.
func AppendObject(dst, body []byte, plan *Plan) ([]byte, error) {
	cols := plan.Columns
	if len(body) < 2 {
		return dst, errShortDataRow
	}
	n := int(int16(uint16(body[0])<<8 | uint16(body[1])))
	if n != len(cols) {
		return dst, fmt.Errorf("rows: column count mismatch (got %d, plan %d)",
			n, len(cols))
	}
	body = body[2:]
	dst = append(dst, '{')
	for i := 0; i < n; i++ {
		if len(body) < 4 {
			return dst, errShortColumnHeader
		}
		l := int32(uint32(body[0])<<24 | uint32(body[1])<<16 |
			uint32(body[2])<<8 | uint32(body[3]))
		body = body[4:]
		var raw []byte
		if l != -1 {
			if l < 0 || int(l) > len(body) {
				return dst, fmt.Errorf("rows: bad column length %d", l)
			}
			raw = body[:l]
			body = body[l:]
		}
		col := &cols[i]
		if i == 0 {
			dst = append(dst, col.KeyPrefix...)
		} else {
			dst = append(dst, col.KeyPrefixComma...)
		}
		dst = col.Encoder(dst, raw)
	}
	dst = append(dst, '}')
	return dst, nil
}

// AppendArray writes the row as a JSON array (used by columnar mode).
func AppendArray(dst, body []byte, plan *Plan) ([]byte, error) {
	cols := plan.Columns
	if len(body) < 2 {
		return dst, errShortDataRow
	}
	n := int(int16(uint16(body[0])<<8 | uint16(body[1])))
	if n != len(cols) {
		return dst, fmt.Errorf("rows: column count mismatch (got %d, plan %d)",
			n, len(cols))
	}
	body = body[2:]
	dst = append(dst, '[')
	for i := 0; i < n; i++ {
		if len(body) < 4 {
			return dst, errShortColumnHeader
		}
		l := int32(uint32(body[0])<<24 | uint32(body[1])<<16 |
			uint32(body[2])<<8 | uint32(body[3]))
		body = body[4:]
		var raw []byte
		if l != -1 {
			if l < 0 || int(l) > len(body) {
				return dst, fmt.Errorf("rows: bad column length %d", l)
			}
			raw = body[:l]
			body = body[l:]
		}
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = cols[i].Encoder(dst, raw)
	}
	dst = append(dst, ']')
	return dst, nil
}

var (
	errShortDataRow      = fmt.Errorf("rows: short DataRow")
	errShortColumnHeader = fmt.Errorf("rows: short column header")
)

// nudge to keep imported even if dead-code-eliminated path changes.
var _ = jsonwriter.AppendNull
