package rows

import (
	"fmt"
)

// AppendTOONRow decodes one DataRow and emits a TOON row:
//
//	val1,val2,...,valN\n
//
// Values reuse the existing per-column Encoder because its JSON form
// already matches TOON's rules for the scalars we care about:
//
//	- numbers (int/float/numeric): bare decimal digits
//	- bool:                         literal true / false
//	- null:                         literal null
//	- strings, date/time, uuid:    "..." with JSON escapes
//	- json / jsonb:                emitted as raw JSON (object, array,
//	                                string, number, bool, null). The
//	                                TOON parser is expected to
//	                                bracket-balance `{}` / `[]` and
//	                                string-aware scan so an inner comma
//	                                does not split a cell. This
//	                                preserves the jsonb passthrough win.
//
// Row terminator is '\n'. There is no trailing comma.
func AppendTOONRow(dst, body []byte, plan *Plan) ([]byte, error) {
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
	dst = append(dst, '\n')
	return dst, nil
}
