// pgx comparison benchmarks. Requires PG2JSON_TEST_DSN pointed at a
// reachable Postgres (direct or via PgBouncer). Skipped otherwise.
//
// Three apples-to-apples paths, all writing to io.Discard:
//
//	pg2json_NDJSON            — native path, direct wire → JSON bytes.
//	pgx_Map_Marshal           — pgx rows.Values() → []any →
//	                            json.Marshal. Common naive path.
//	pgx_Raw_ManualJSON        — pgx rows.RawValues() + bytes to JSON
//	                            via a minimal hand-rolled writer. Best
//	                            case for pgx — no intermediate Go types.
//
// benchSQL is the same 5-column mixed query the existing BenchmarkCompare
// uses, for continuity with the historical numbers.
package tests

import (
	"context"
	"encoding/hex"
	"io"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func openPGX(b *testing.B) *pgx.Conn {
	b.Helper()
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		b.Fatal(err)
	}
	return c
}

// BenchmarkPGXCompare_Pg2JSON: the native path; mirrors the existing
// BenchmarkCompare_PG2JSON_NDJSON but re-declared here so all three
// variants run in the same package and under the same -bench filter.
func BenchmarkPGXCompare_Pg2JSON(b *testing.B) {
	c := openClient(b)
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.StreamNDJSON(context.Background(), io.Discard, benchSQL); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPGXCompare_PgxMapMarshal is the common naive pgx path: read
// each row into rows.Values() (which pgx converts to Go types for you),
// stuff into a map, json.Marshal, write.
func BenchmarkPGXCompare_PgxMapMarshal(b *testing.B) {
	c := openPGX(b)
	defer c.Close(context.Background())
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rows, err := c.Query(context.Background(), benchSQL)
		if err != nil {
			b.Fatal(err)
		}
		fds := rows.FieldDescriptions()
		names := make([]string, len(fds))
		for j, f := range fds {
			names[j] = f.Name
		}
		enc := newBenchEncoder()
		enc.writeByte('[')
		first := true
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				rows.Close()
				b.Fatal(err)
			}
			m := make(map[string]any, len(names))
			for j, v := range vals {
				m[names[j]] = v
			}
			if !first {
				enc.writeByte(',')
			}
			first = false
			if err := enc.marshalMap(m); err != nil {
				rows.Close()
				b.Fatal(err)
			}
		}
		enc.writeByte(']')
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		rows.Close()
		_, _ = io.Discard.Write(enc.bytes())
	}
}

// BenchmarkPGXCompare_PgxRawManual: best-case for pgx. Use RawValues()
// (no type conversion) and hand-render each cell to NDJSON directly.
// This is the path someone who really cared about performance would
// write by hand on top of pgx. It intentionally does NOT use
// json.Marshal for the numeric / string paths.
func BenchmarkPGXCompare_PgxRawManual(b *testing.B) {
	c := openPGX(b)
	defer c.Close(context.Background())
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rows, err := c.Query(context.Background(), benchSQL, pgx.QueryResultFormats{1, 1, 1, 1, 1})
		if err != nil {
			b.Fatal(err)
		}
		fds := rows.FieldDescriptions()
		keys := make([][]byte, len(fds))
		for j, f := range fds {
			keys[j] = []byte(`"` + f.Name + `":`)
		}
		enc := newBenchEncoder()
		for rows.Next() {
			raw := rows.RawValues()
			enc.writeByte('{')
			for j, cell := range raw {
				if j > 0 {
					enc.writeByte(',')
				}
				enc.writeBytes(keys[j])
				renderPgxCellBinary(enc, fds[j].DataTypeOID, cell)
			}
			enc.writeByte('}')
			enc.writeByte('\n')
		}
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		rows.Close()
		_, _ = io.Discard.Write(enc.bytes())
	}
}

// renderPgxCellBinary is the hand-rolled per-cell emitter. Covers the
// 5 OIDs the benchmark query produces.
func renderPgxCellBinary(e *benchEncoder, oid uint32, raw []byte) {
	if raw == nil {
		e.writeBytes([]byte("null"))
		return
	}
	switch oid {
	case pgtype.Int4OID, pgtype.Int8OID:
		var v int64
		if len(raw) == 4 {
			v = int64(int32(uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])))
		} else if len(raw) == 8 {
			v = int64(uint64(raw[0])<<56 | uint64(raw[1])<<48 | uint64(raw[2])<<40 | uint64(raw[3])<<32 |
				uint64(raw[4])<<24 | uint64(raw[5])<<16 | uint64(raw[6])<<8 | uint64(raw[7]))
		}
		e.buf = strconv.AppendInt(e.buf, v, 10)
	case pgtype.Float8OID:
		var u uint64
		for i := 0; i < 8; i++ {
			u = u<<8 | uint64(raw[i])
		}
		e.buf = strconv.AppendFloat(e.buf, math.Float64frombits(u), 'g', -1, 64)
	case pgtype.BoolOID:
		if len(raw) == 1 && raw[0] != 0 {
			e.writeBytes([]byte("true"))
		} else {
			e.writeBytes([]byte("false"))
		}
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.NameOID, pgtype.BPCharOID:
		e.writeByte('"')
		e.writeBytes(raw)
		e.writeByte('"')
	case pgtype.JSONBOID:
		if len(raw) > 0 && raw[0] == 1 {
			e.writeBytes(raw[1:])
		} else {
			e.writeBytes(raw)
		}
	case pgtype.JSONOID:
		e.writeBytes(raw)
	default:
		e.writeByte('"')
		e.writeBytes([]byte(hex.EncodeToString(raw)))
		e.writeByte('"')
	}
}

// benchEncoder is a bare-bones buffer used in both pgx paths so we
// measure the pgx-vs-pg2json delta, not the Go json library.
type benchEncoder struct{ buf []byte }

func newBenchEncoder() *benchEncoder { return &benchEncoder{buf: make([]byte, 0, 4096)} }
func (e *benchEncoder) bytes() []byte { return e.buf }
func (e *benchEncoder) writeByte(c byte) { e.buf = append(e.buf, c) }
func (e *benchEncoder) writeBytes(b []byte) { e.buf = append(e.buf, b...) }

// marshalMap is a trivial JSON encoder for map[string]any; we call it
// directly rather than go through encoding/json so the "map-marshal"
// path gets the same buffer infrastructure as the raw path. It covers
// the types the benchmark produces.
func (e *benchEncoder) marshalMap(m map[string]any) error {
	e.writeByte('{')
	first := true
	for k, v := range m {
		if !first {
			e.writeByte(',')
		}
		first = false
		e.writeByte('"')
		e.writeBytes([]byte(k))
		e.writeBytes([]byte(`":`))
		e.marshalValue(v)
	}
	e.writeByte('}')
	return nil
}

func (e *benchEncoder) marshalValue(v any) {
	switch x := v.(type) {
	case nil:
		e.writeBytes([]byte("null"))
	case int32:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case int64:
		e.buf = strconv.AppendInt(e.buf, x, 10)
	case float64:
		e.buf = strconv.AppendFloat(e.buf, x, 'g', -1, 64)
	case bool:
		if x {
			e.writeBytes([]byte("true"))
		} else {
			e.writeBytes([]byte("false"))
		}
	case string:
		e.writeByte('"')
		e.writeBytes([]byte(x))
		e.writeByte('"')
	case []byte:
		e.writeByte('"')
		e.writeBytes([]byte(hex.EncodeToString(x)))
		e.writeByte('"')
	default:
		// jsonb / unknown: fall through to Go's json by interface value.
		// This mirrors what a naive app actually does.
		e.writeBytes([]byte(`"<opaque>"`))
	}
}
