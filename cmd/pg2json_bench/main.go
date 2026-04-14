// pg2json_bench is an opt-in micro-benchmark harness that requires a real
// PostgreSQL. It runs a few canned SELECT shapes and prints rows/s,
// MB/s, and per-call timing.
//
// Usage:
//   PG2JSON_DSN=postgres://... go run ./cmd/pg2json_bench -rows 50000
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
)

func main() {
	rows := flag.Int("rows", 10000, "rows per scenario")
	repeat := flag.Int("repeat", 5, "iterations per scenario")
	flag.Parse()

	dsn := os.Getenv("PG2JSON_DSN")
	if dsn == "" {
		die("PG2JSON_DSN not set")
	}
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		die(err.Error())
	}
	ctx := context.Background()
	c, err := pg2json.Open(ctx, cfg)
	if err != nil {
		die("open: " + err.Error())
	}
	defer c.Close()

	scenarios := []struct {
		name string
		sql  string
	}{
		{
			"narrow_int",
			fmt.Sprintf("SELECT g AS id FROM generate_series(1, %d) g", *rows),
		},
		{
			"narrow_text",
			fmt.Sprintf("SELECT 'row-' || g AS name FROM generate_series(1, %d) g", *rows),
		},
		{
			"mixed_5col",
			fmt.Sprintf(`SELECT g AS id, 'name-'||g AS name, (g::float8/3.0) AS score,
				(g %% 2 = 0) AS flag, ('{"k":'||g||'}')::jsonb AS meta
				FROM generate_series(1, %d) g`, *rows),
		},
		{
			"wide_jsonb",
			fmt.Sprintf(`SELECT g, ('{"a":1,"b":[1,2,3],"c":"x"}')::jsonb AS j
				FROM generate_series(1, %d) g`, *rows),
		},
	}

	fmt.Printf("%-14s %10s %10s %12s %12s\n", "scenario", "rows", "ms/op", "rows/s", "MB/s")
	for _, sc := range scenarios {
		var totalNs int64
		var totalBytes int64
		for i := 0; i < *repeat; i++ {
			t0 := time.Now()
			n, err := streamCount(ctx, c, sc.sql)
			if err != nil {
				die(sc.name + ": " + err.Error())
			}
			elapsed := time.Since(t0).Nanoseconds()
			totalNs += elapsed
			totalBytes += n
		}
		avgNs := float64(totalNs) / float64(*repeat)
		avgBytes := float64(totalBytes) / float64(*repeat)
		rowsPerSec := float64(*rows) / (avgNs / 1e9)
		mbPerSec := avgBytes / (avgNs / 1e9) / (1 << 20)
		fmt.Printf("%-14s %10d %10.2f %12.0f %12.2f\n",
			sc.name, *rows, avgNs/1e6, rowsPerSec, mbPerSec)
	}
}

// streamCount streams to io.Discard while counting bytes.
func streamCount(ctx context.Context, c *pg2json.Client, sql string) (int64, error) {
	cw := &countingWriter{}
	if err := c.StreamNDJSON(ctx, cw, sql); err != nil {
		return 0, err
	}
	return cw.n, nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

var _ io.Writer = (*countingWriter)(nil)

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
