package tests

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// BenchmarkOutputModes compares pg2json's three output formats
// (NDJSON / Columnar / TOON) against pgx+RawValues on the same 5
// shapes. Each writes to io.Discard; we report ns/op, MB/s, allocs,
// and bytes per row to make the size tradeoff explicit.
func BenchmarkOutputModes(b *testing.B) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		b.Skip("PG2JSON_TEST_DSN not set")
	}
	shapes := benchShapes
	const n = 100000
	for _, sh := range shapes {
		sql := sh.sql(n)

		b.Run(fmt.Sprintf("%s/NDJSON", sh.name), func(b *testing.B) {
			c := openClient(b)
			defer c.Close()
			b.ReportAllocs()
			b.ResetTimer()
			var total int64
			for i := 0; i < b.N; i++ {
				cw := &countingWriterBytes{}
				if err := c.StreamNDJSON(context.Background(), cw, sql); err != nil {
					b.Fatal(err)
				}
				total += cw.n
			}
			b.SetBytes(total / int64(b.N))
			b.ReportMetric(float64(total)/float64(b.N)/float64(n), "B/row")
		})

		b.Run(fmt.Sprintf("%s/Columnar", sh.name), func(b *testing.B) {
			c := openClient(b)
			defer c.Close()
			b.ReportAllocs()
			b.ResetTimer()
			var total int64
			for i := 0; i < b.N; i++ {
				cw := &countingWriterBytes{}
				if err := c.StreamColumnar(context.Background(), cw, sql); err != nil {
					b.Fatal(err)
				}
				total += cw.n
			}
			b.SetBytes(total / int64(b.N))
			b.ReportMetric(float64(total)/float64(b.N)/float64(n), "B/row")
		})

		b.Run(fmt.Sprintf("%s/TOON", sh.name), func(b *testing.B) {
			c := openClient(b)
			defer c.Close()
			b.ReportAllocs()
			b.ResetTimer()
			var total int64
			for i := 0; i < b.N; i++ {
				cw := &countingWriterBytes{}
				if err := c.StreamTOON(context.Background(), cw, sql); err != nil {
					b.Fatal(err)
				}
				total += cw.n
			}
			b.SetBytes(total / int64(b.N))
			b.ReportMetric(float64(total)/float64(b.N)/float64(n), "B/row")
		})

		b.Run(fmt.Sprintf("%s/PgxRaw", sh.name), func(b *testing.B) {
			c, err := pgx.Connect(context.Background(), dsn)
			if err != nil {
				b.Fatal(err)
			}
			defer c.Close(context.Background())
			b.ReportAllocs()
			b.ResetTimer()
			var total int64
			for i := 0; i < b.N; i++ {
				got, err := runPgxRaw(c, sql, sh.fmts, io.Discard)
				if err != nil {
					b.Fatal(err)
				}
				total += got
			}
			b.SetBytes(total / int64(b.N))
			b.ReportMetric(float64(total)/float64(b.N)/float64(n), "B/row")
		})
	}
}
