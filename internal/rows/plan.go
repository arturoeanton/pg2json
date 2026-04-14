// Package rows compiles a per-shape encoder plan from a RowDescription
// and provides the hot row loop that writes JSON.
package rows

import (
	"encoding/binary"
	"fmt"

	"github.com/arturoeanton/pg2json/internal/jsonwriter"
	"github.com/arturoeanton/pg2json/internal/protocol"
	"github.com/arturoeanton/pg2json/internal/types"
)

// Column captures the parsed RowDescription field plus its compiled encoder.
type Column struct {
	Name      string
	TableOID  uint32
	AttrNum   int16
	TypeOID   protocol.OID
	TypeMod   int32
	TypeLen   int16
	Format    int16
	Encoder   types.Encoder
	KeyPrefix []byte // pre-built `"name":`
}

// Plan is the cached per-shape execution plan for a result set.
type Plan struct {
	Columns []Column
	// ColumnsArrayHeader is the JSON header for the columnar mode:
	// `{"columns":["a","b"],"rows":[`
	ColumnsArrayHeader []byte
}

// ApplyFormats rewrites the encoder for each column to match the supplied
// per-column format codes. Used after Bind asks the server for binary on
// some columns.
func (p *Plan) ApplyFormats(formats []int16) {
	for i := range p.Columns {
		var f int16
		if len(formats) == 1 {
			f = formats[0]
		} else if len(formats) == len(p.Columns) {
			f = formats[i]
		}
		p.Columns[i].Format = f
		if f == 1 {
			if be := types.PickBinary(p.Columns[i].TypeOID); be != nil {
				p.Columns[i].Encoder = be
			}
		} else {
			p.Columns[i].Encoder = types.Pick(p.Columns[i].TypeOID)
		}
	}
}

// ParseRowDescriptionForFormats parses a 'T' body and applies binary
// encoders for any column whose format code (in `formats`) is 1.
func ParseRowDescriptionForFormats(body []byte, formats []int16) (*Plan, error) {
	p, err := ParseRowDescription(body)
	if err != nil {
		return nil, err
	}
	p.ApplyFormats(formats)
	return p, nil
}

// PickResultFormats returns one int16 per column describing the format we
// want the server to send (1=binary, 0=text), based on which OIDs have
// specialised binary encoders.
func PickResultFormats(cols []Column) []int16 {
	out := make([]int16, len(cols))
	for i, c := range cols {
		if types.HasBinary(c.TypeOID) {
			out[i] = 1
		}
	}
	return out
}

// ParseRowDescription reads a 'T' message body and returns a compiled Plan.
//
// Body layout (server -> client):
//   int16  field count
//   for each field:
//     cstring name
//     int32   table OID  (0 if not from a table)
//     int16   attribute number
//     int32   type OID
//     int16   type size
//     int32   type modifier
//     int16   format code
func ParseRowDescription(body []byte) (*Plan, error) {
	if len(body) < 2 {
		return nil, fmt.Errorf("rows: short RowDescription")
	}
	n := int(int16(binary.BigEndian.Uint16(body[:2])))
	if n < 0 {
		return nil, fmt.Errorf("rows: negative field count %d", n)
	}
	body = body[2:]

	cols := make([]Column, n)
	for i := 0; i < n; i++ {
		// name (cstring)
		end := indexZero(body)
		if end < 0 {
			return nil, fmt.Errorf("rows: unterminated column name")
		}
		name := string(body[:end])
		body = body[end+1:]
		if len(body) < 18 {
			return nil, fmt.Errorf("rows: short column descriptor")
		}
		c := &cols[i]
		c.Name = name
		c.TableOID = binary.BigEndian.Uint32(body[0:4])
		c.AttrNum = int16(binary.BigEndian.Uint16(body[4:6]))
		c.TypeOID = protocol.OID(binary.BigEndian.Uint32(body[6:10]))
		c.TypeLen = int16(binary.BigEndian.Uint16(body[10:12]))
		c.TypeMod = int32(binary.BigEndian.Uint32(body[12:16]))
		c.Format = int16(binary.BigEndian.Uint16(body[16:18]))
		body = body[18:]

		// Phase 1: text encoders only. If the server sends binary (it
		// never will from a simple Query, and we always request text in
		// the extended protocol), we still pick the text encoder; that
		// would render hex-looking garbage but never produce invalid JSON.
		c.Encoder = types.Pick(c.TypeOID)
		c.KeyPrefix = jsonwriter.AppendKey(nil, name)
	}

	p := &Plan{Columns: cols}
	p.ColumnsArrayHeader = buildColumnsHeader(cols)
	return p, nil
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func buildColumnsHeader(cols []Column) []byte {
	buf := make([]byte, 0, 32+len(cols)*16)
	buf = append(buf, '{', '"', 'c', 'o', 'l', 'u', 'm', 'n', 's', '"', ':', '[')
	for i, c := range cols {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = jsonwriter.AppendString(buf, c.Name)
	}
	buf = append(buf, ']', ',', '"', 'r', 'o', 'w', 's', '"', ':', '[')
	return buf
}
