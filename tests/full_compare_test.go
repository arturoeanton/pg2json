package tests

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/arturoeanton/pg2json/pg2json"
	_ "github.com/arturoeanton/pg2json/pg2json/stdlib"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// BenchmarkFullCompare runs every pg2json output path against every
// comparable pgx path on the same shape (bench_mixed_5col, 100k rows).
// Reports ns/op, allocs/op, bytes/op for each. The goal is one table
// to answer "where do we win / where do we lose" at a glance.
//
// Shape: id int4, name text, score float8, flag bool, meta jsonb.
func BenchmarkFullCompare(b *testing.B) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}
	const sql100k = "SELECT id, name, score, flag, meta FROM bench_mixed_5col"

	// ---------- pg2json: JSON output modes -----------------------------

	b.Run("pg2json/QueryJSON", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := c.QueryJSON(context.Background(), sql100k)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pg2json/StreamNDJSON", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := c.StreamNDJSON(context.Background(), io.Discard, sql100k); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pg2json/StreamColumnar", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := c.StreamColumnar(context.Background(), io.Discard, sql100k); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pg2json/StreamTOON", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := c.StreamTOON(context.Background(), io.Discard, sql100k); err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---------- pg2json: struct scan -----------------------------------

	type mixedRow struct {
		Id    int32
		Name  string
		Score float64
		Flag  bool
		Meta  json.RawMessage
	}

	b.Run("pg2json/ScanStruct", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := pg2json.ScanStruct[mixedRow](c, context.Background(), sql100k)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pg2json/ScanStructBatched", func(b *testing.B) {
		c := openClient(b)
		defer c.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			err := pg2json.ScanStructBatched[mixedRow](c, context.Background(), 10000,
				func(batch []mixedRow) error { return nil },
				sql100k)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---------- pg2json: database/sql adapter --------------------------

	b.Run("pg2json/stdlib.Query", func(b *testing.B) {
		db, err := sql.Open("pg2json", dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rs, err := db.Query(sql100k)
			if err != nil {
				b.Fatal(err)
			}
			var r mixedRow
			for rs.Next() {
				if err := rs.Scan(&r.Id, &r.Name, &r.Score, &r.Flag, &r.Meta); err != nil {
					b.Fatal(err)
				}
			}
			rs.Close()
			if err := rs.Err(); err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---------- pgx: native paths --------------------------------------

	b.Run("pgx/Map+Marshal (NDJSON)", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := runPgxMap(c, sql100k, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pgx/RawValues (NDJSON hand)", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		fmts := []int16{1, 1, 1, 1, 1}
		for i := 0; i < b.N; i++ {
			if _, err := runPgxRaw(c, sql100k, fmts, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pgx/Scan(manual)", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rs, err := c.Query(context.Background(), sql100k)
			if err != nil {
				b.Fatal(err)
			}
			var r mixedRow
			for rs.Next() {
				if err := rs.Scan(&r.Id, &r.Name, &r.Score, &r.Flag, &r.Meta); err != nil {
					b.Fatal(err)
				}
			}
			rs.Close()
			if err := rs.Err(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pgx/CollectByName", func(b *testing.B) {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close(context.Background())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rs, err := c.Query(context.Background(), sql100k)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := pgx.CollectRows(rs, pgx.RowToStructByName[mixedRow]); err != nil {
				b.Fatal(err)
			}
		}
	})

	// ---------- pgx: database/sql adapter ------------------------------

	b.Run("pgx/stdlib.Query", func(b *testing.B) {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rs, err := db.Query(sql100k)
			if err != nil {
				b.Fatal(err)
			}
			var r mixedRow
			for rs.Next() {
				if err := rs.Scan(&r.Id, &r.Name, &r.Score, &r.Flag, &r.Meta); err != nil {
					b.Fatal(err)
				}
			}
			rs.Close()
			if err := rs.Err(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
