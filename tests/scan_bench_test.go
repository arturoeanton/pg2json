package tests

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
	"github.com/jackc/pgx/v5"
)

// Row shapes matched to the docker bench tables. Field names match
// column names case-insensitively so no tags are needed.
type benchMixedRow struct {
	Id    int32
	Name  string
	Score float64
	Flag  bool
	Meta  json.RawMessage
}

type benchNarrowRow struct {
	Id int32
}

type benchWideRow struct {
	Id int32
	J  json.RawMessage
}

// Compares pg2json.ScanStruct against pgx row-scanning paths. Two pgx
// variants are measured:
//
//	PgxScan      — pgx.Conn.Query() + rows.Scan(&f1, &f2, ...) row-by-row.
//	PgxCollect   — pgx.CollectRows(rows, pgx.RowToStructByName[T]).
//
// All read 100 000 rows from the seeded docker tables. Binary format is
// requested on both sides so the wire bytes are identical.
func BenchmarkScanMixed(b *testing.B) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}

	const sql = "SELECT id, name, score, flag, meta FROM bench_mixed_5col"

	b.Run("Pg2JSON_ScanStruct", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		var rowsTotal int64
		for i := 0; i < b.N; i++ {
			out, err := pg2json.ScanStruct[benchMixedRow](c, context.Background(), sql)
			if err != nil {
				b.Fatal(err)
			}
			rowsTotal += int64(len(out))
		}
		b.ReportMetric(float64(rowsTotal)/b.Elapsed().Seconds(), "rows/s")
	})

	b.Run("PgxScan", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		var rowsTotal int64
		for i := 0; i < b.N; i++ {
			rs, err := c.Query(context.Background(), sql)
			if err != nil {
				b.Fatal(err)
			}
			var count int64
			for rs.Next() {
				var r benchMixedRow
				if err := rs.Scan(&r.Id, &r.Name, &r.Score, &r.Flag, &r.Meta); err != nil {
					b.Fatal(err)
				}
				count++
			}
			rs.Close()
			if err := rs.Err(); err != nil {
				b.Fatal(err)
			}
			rowsTotal += count
		}
		b.ReportMetric(float64(rowsTotal)/b.Elapsed().Seconds(), "rows/s")
	})

	b.Run("PgxCollectByName", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		var rowsTotal int64
		for i := 0; i < b.N; i++ {
			rs, err := c.Query(context.Background(), sql)
			if err != nil {
				b.Fatal(err)
			}
			out, err := pgx.CollectRows(rs, pgx.RowToStructByName[benchMixedRow])
			if err != nil {
				b.Fatal(err)
			}
			rowsTotal += int64(len(out))
		}
		b.ReportMetric(float64(rowsTotal)/b.Elapsed().Seconds(), "rows/s")
	})
}

func BenchmarkScanNarrow(b *testing.B) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}
	const sql = "SELECT id FROM bench_narrow_int"

	b.Run("Pg2JSON_ScanStruct", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := pg2json.ScanStruct[benchNarrowRow](c, context.Background(), sql); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PgxCollectByName", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rs, err := c.Query(context.Background(), sql)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := pgx.CollectRows(rs, pgx.RowToStructByName[benchNarrowRow]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkScanWideJSONB(b *testing.B) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}
	const sql = "SELECT id, j FROM bench_wide_jsonb"

	b.Run("Pg2JSON_ScanStruct", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := pg2json.ScanStruct[benchWideRow](c, context.Background(), sql); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("PgxCollectByName", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rs, err := c.Query(context.Background(), sql)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := pgx.CollectRows(rs, pgx.RowToStructByName[benchWideRow]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// keep time import referenced for future timestamp bench
var _ = time.Time{}
