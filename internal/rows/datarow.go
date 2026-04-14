package rows

import (
	"encoding/binary"
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
func AppendObject(dst, body []byte, plan *Plan) ([]byte, error) {
	cols, body, err := readColumnCount(body, plan)
	if err != nil {
		return dst, err
	}
	dst = append(dst, '{')
	if cols > 0 {
		// First column: bare KeyPrefix (no leading comma).
		raw, rest, err := readColumn(body)
		if err != nil {
			return dst, err
		}
		body = rest
		col := &plan.Columns[0]
		dst = append(dst, col.KeyPrefix...)
		dst = col.Encoder(dst, raw)
		// Remaining columns: KeyPrefixComma (leading ',' fused in).
		for i := 1; i < cols; i++ {
			raw, rest, err = readColumn(body)
			if err != nil {
				return dst, err
			}
			body = rest
			col = &plan.Columns[i]
			dst = append(dst, col.KeyPrefixComma...)
			dst = col.Encoder(dst, raw)
		}
	}
	dst = append(dst, '}')
	return dst, nil
}

// AppendArray writes the row as a JSON array (used by columnar mode).
func AppendArray(dst, body []byte, plan *Plan) ([]byte, error) {
	cols, body, err := readColumnCount(body, plan)
	if err != nil {
		return dst, err
	}
	dst = append(dst, '[')
	if cols > 0 {
		raw, rest, err := readColumn(body)
		if err != nil {
			return dst, err
		}
		body = rest
		dst = plan.Columns[0].Encoder(dst, raw)
		for i := 1; i < cols; i++ {
			raw, rest, err = readColumn(body)
			if err != nil {
				return dst, err
			}
			body = rest
			dst = append(dst, ',')
			dst = plan.Columns[i].Encoder(dst, raw)
		}
	}
	dst = append(dst, ']')
	return dst, nil
}

func readColumnCount(body []byte, plan *Plan) (int, []byte, error) {
	if len(body) < 2 {
		return 0, nil, fmt.Errorf("rows: short DataRow")
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	if n != len(plan.Columns) {
		return 0, nil, fmt.Errorf("rows: column count mismatch (got %d, plan %d)",
			n, len(plan.Columns))
	}
	return n, body[2:], nil
}

func readColumn(body []byte) (raw, rest []byte, err error) {
	if len(body) < 4 {
		return nil, nil, fmt.Errorf("rows: short column header")
	}
	l := int32(binary.BigEndian.Uint32(body[:4]))
	body = body[4:]
	if l == -1 {
		return nil, body, nil
	}
	if l < 0 || int(l) > len(body) {
		return nil, nil, fmt.Errorf("rows: bad column length %d", l)
	}
	return body[:l], body[l:], nil
}

// nudge to keep imported even if dead-code-eliminated path changes.
var _ = jsonwriter.AppendNull
