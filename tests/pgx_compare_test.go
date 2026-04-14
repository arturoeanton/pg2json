// Apples-to-apples comparison: pg2json vs pgx on the same wire, same PG,
// same shapes. All benches write NDJSON to io.Discard. Requires
// PG2JSON_TEST_DSN.
//
// Three paths:
//
//	Pg2JSON        — native StreamNDJSON.
//	PgxMap         — pgx rows.Values() → map[string]any → json.Marshal.
//	                 The naive-but-common path.
//	PgxRaw         — pgx rows.RawValues() + hand-written NDJSON encoder.
//	                 Best case for pgx; no intermediate Go types.
//
// Five shapes, three sizes each:
//
//	narrow_int   : single int4 column.
//	mixed_5col   : id int4, name text, score float8, flag bool, meta jsonb.
//	wide_jsonb   : id int4, j jsonb (canonical 3-field object).
//	array_int    : id int4, arr int4[] of 10 elements (tests array decoder).
//	null_heavy   : id int4, v text where v is NULL for 50% of rows.
package tests

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type shape struct {
	name string
	sql  func(n int) string
	fmts []int16 // per-column binary formats for pgx Raw path
}

var benchShapes = []shape{
	{
		"narrow_int",
		func(n int) string {
			return fmt.Sprintf("SELECT g::int4 AS id FROM generate_series(1, %d) g", n)
		},
		[]int16{1},
	},
	{
		"mixed_5col",
		func(n int) string {
			return fmt.Sprintf(`SELECT g::int4 AS id, 'name-'||g AS name,
				(g::float8 / 3.0) AS score, (g %% 2 = 0) AS flag,
				('{"k":'||g||'}')::jsonb AS meta
				FROM generate_series(1, %d) g`, n)
		},
		[]int16{1, 1, 1, 1, 1},
	},
	{
		"wide_jsonb",
		func(n int) string {
			return fmt.Sprintf(`SELECT g::int4 AS id,
				('{"a":1,"b":[1,2,3],"c":"x"}')::jsonb AS j
				FROM generate_series(1, %d) g`, n)
		},
		[]int16{1, 1},
	},
	{
		"array_int",
		func(n int) string {
			return fmt.Sprintf(`SELECT g::int4 AS id,
				ARRAY[g,g+1,g+2,g+3,g+4,g+5,g+6,g+7,g+8,g+9]::int4[] AS arr
				FROM generate_series(1, %d) g`, n)
		},
		[]int16{1, 1},
	},
	{
		"null_heavy",
		func(n int) string {
			return fmt.Sprintf(`SELECT g::int4 AS id,
				CASE WHEN g %% 2 = 0 THEN NULL ELSE 'row-'||g END AS v
				FROM generate_series(1, %d) g`, n)
		},
		[]int16{1, 1},
	},
}

var benchSizes = []int{1000, 10000, 100000}

func BenchmarkPgx(b *testing.B) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}
	for _, sh := range benchShapes {
		for _, n := range benchSizes {
			sql := sh.sql(n)
			b.Run(fmt.Sprintf("%s/%d/Pg2JSON", sh.name, n), func(b *testing.B) {
				c := openClient(b)
				defer c.Close()
				b.ReportAllocs()
				b.ResetTimer()
				var bytesSum int64
				for i := 0; i < b.N; i++ {
					cw := &countingWriterBytes{}
					if err := c.StreamNDJSON(context.Background(), cw, sql); err != nil {
						b.Fatal(err)
					}
					bytesSum += cw.n
				}
				b.SetBytes(bytesSum / int64(b.N))
			})
			b.Run(fmt.Sprintf("%s/%d/PgxMap", sh.name, n), func(b *testing.B) {
				c := pgxConnect(b, dsn)
				defer c.Close(context.Background())
				b.ReportAllocs()
				b.ResetTimer()
				var bytesSum int64
				for i := 0; i < b.N; i++ {
					n, err := runPgxMap(c, sql, io.Discard)
					if err != nil {
						b.Fatal(err)
					}
					bytesSum += n
				}
				b.SetBytes(bytesSum / int64(b.N))
			})
			b.Run(fmt.Sprintf("%s/%d/PgxRaw", sh.name, n), func(b *testing.B) {
				c := pgxConnect(b, dsn)
				defer c.Close(context.Background())
				b.ReportAllocs()
				b.ResetTimer()
				var bytesSum int64
				for i := 0; i < b.N; i++ {
					n, err := runPgxRaw(c, sql, sh.fmts, io.Discard)
					if err != nil {
						b.Fatal(err)
					}
					bytesSum += n
				}
				b.SetBytes(bytesSum / int64(b.N))
			})
		}
	}
}

func pgxConnect(b *testing.B, dsn string) *pgx.Conn {
	b.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		b.Fatal(err)
	}
	return c
}

type countingWriterBytes struct{ n int64 }

func (c *countingWriterBytes) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// runPgxMap: pgx Values() → map[string]any → json.Marshal per row.
// Classic "I use pgx and encoding/json" pattern.
func runPgxMap(c *pgx.Conn, sql string, w io.Writer) (int64, error) {
	rows, err := c.Query(context.Background(), sql)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	names := make([]string, len(fds))
	for i, f := range fds {
		names[i] = f.Name
	}
	var total int64
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return total, err
		}
		m := make(map[string]any, len(names))
		for i, v := range vals {
			m[names[i]] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			return total, err
		}
		n, err := w.Write(b)
		total += int64(n)
		if err != nil {
			return total, err
		}
		n, err = w.Write([]byte("\n"))
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, rows.Err()
}

// runPgxRaw: pgx RawValues() + hand-written NDJSON encoder. Requests
// binary format for every column.
func runPgxRaw(c *pgx.Conn, sql string, fmts []int16, w io.Writer) (int64, error) {
	rows, err := c.Query(context.Background(), sql, pgx.QueryResultFormats(fmts))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	keys := make([][]byte, len(fds))
	oids := make([]uint32, len(fds))
	for i, f := range fds {
		keys[i] = []byte(`"` + f.Name + `":`)
		oids[i] = f.DataTypeOID
	}
	buf := make([]byte, 0, 64*1024)
	var total int64
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		n, err := w.Write(buf)
		total += int64(n)
		buf = buf[:0]
		return err
	}
	for rows.Next() {
		raw := rows.RawValues()
		buf = append(buf, '{')
		for i, cell := range raw {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, keys[i]...)
			buf = appendRawCell(buf, oids[i], cell)
		}
		buf = append(buf, '}', '\n')
		if len(buf) >= 32*1024 {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, rows.Err()
}

func appendRawCell(dst []byte, oid uint32, raw []byte) []byte {
	if raw == nil {
		return append(dst, 'n', 'u', 'l', 'l')
	}
	switch oid {
	case pgtype.Int2OID:
		v := int16(uint16(raw[0])<<8 | uint16(raw[1]))
		return strconv.AppendInt(dst, int64(v), 10)
	case pgtype.Int4OID, pgtype.OIDOID:
		v := int32(uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]))
		return strconv.AppendInt(dst, int64(v), 10)
	case pgtype.Int8OID:
		v := int64(uint64(raw[0])<<56 | uint64(raw[1])<<48 | uint64(raw[2])<<40 | uint64(raw[3])<<32 |
			uint64(raw[4])<<24 | uint64(raw[5])<<16 | uint64(raw[6])<<8 | uint64(raw[7]))
		return strconv.AppendInt(dst, v, 10)
	case pgtype.Float8OID:
		u := uint64(raw[0])<<56 | uint64(raw[1])<<48 | uint64(raw[2])<<40 | uint64(raw[3])<<32 |
			uint64(raw[4])<<24 | uint64(raw[5])<<16 | uint64(raw[6])<<8 | uint64(raw[7])
		return strconv.AppendFloat(dst, math.Float64frombits(u), 'g', -1, 64)
	case pgtype.BoolOID:
		if len(raw) == 1 && raw[0] != 0 {
			return append(dst, 't', 'r', 'u', 'e')
		}
		return append(dst, 'f', 'a', 'l', 's', 'e')
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.NameOID, pgtype.BPCharOID:
		dst = append(dst, '"')
		dst = appendEscaped(dst, raw)
		return append(dst, '"')
	case pgtype.JSONBOID:
		if len(raw) > 0 && raw[0] == 1 {
			return append(dst, raw[1:]...)
		}
		return append(dst, raw...)
	case pgtype.JSONOID:
		return append(dst, raw...)
	case pgtype.Int4ArrayOID:
		return appendInt4ArrayBinary(dst, raw)
	case pgtype.ByteaOID:
		dst = append(dst, '"', '\\', '\\', 'x')
		dst = append(dst, []byte(hex.EncodeToString(raw))...)
		return append(dst, '"')
	default:
		dst = append(dst, '"')
		dst = append(dst, raw...)
		return append(dst, '"')
	}
}

func appendEscaped(dst, src []byte) []byte {
	// Minimal escaper matching JSON requirements — same approach as
	// pg2json's jsonwriter but tiny, for the pgx bench only.
	for _, c := range src {
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexLower[c>>4], hexLower[c&0xF])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

const hexLower = "0123456789abcdef"

func appendInt4ArrayBinary(dst, raw []byte) []byte {
	if len(raw) < 12 {
		return append(dst, '[', ']')
	}
	ndim := int32(uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]))
	if ndim != 1 {
		return append(dst, '[', ']')
	}
	size := int32(uint32(raw[12])<<24 | uint32(raw[13])<<16 | uint32(raw[14])<<8 | uint32(raw[15]))
	body := raw[20:]
	dst = append(dst, '[')
	off := 0
	for i := int32(0); i < size; i++ {
		if i > 0 {
			dst = append(dst, ',')
		}
		elemLen := int32(uint32(body[off])<<24 | uint32(body[off+1])<<16 |
			uint32(body[off+2])<<8 | uint32(body[off+3]))
		off += 4
		if elemLen < 0 {
			dst = append(dst, 'n', 'u', 'l', 'l')
			continue
		}
		v := int32(uint32(body[off])<<24 | uint32(body[off+1])<<16 |
			uint32(body[off+2])<<8 | uint32(body[off+3]))
		off += int(elemLen)
		dst = strconv.AppendInt(dst, int64(v), 10)
	}
	return append(dst, ']')
}
