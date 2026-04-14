// Comparison benchmarks: pg2json's direct path vs the equivalent
// "scan + encoding/json" path. Both connect to the same Postgres,
// run the same query, and write to io.Discard. Skipped unless
// PG2JSON_TEST_DSN is set.
//
// We deliberately do NOT depend on pgx or the pq driver — the comparison
// against database/sql would conflate driver overhead with serialisation
// overhead. Instead we use pg2json itself to read the rows, then take the
// equivalent "decode each cell, build a Go map, json.Marshal" path so the
// only variable is the JSON serialisation strategy.
package tests

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

const benchSQL = `
	SELECT g AS id,
	       'name-'||g AS name,
	       (g::float8 / 3.0) AS score,
	       (g % 2 = 0) AS flag,
	       ('{"k":'||g||'}')::jsonb AS meta
	FROM generate_series(1, 5000) g`

func BenchmarkCompare_PG2JSON_NDJSON(b *testing.B) {
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

func BenchmarkCompare_PG2JSON_Buffered(b *testing.B) {
	c := openClient(b)
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, err := c.QueryJSON(context.Background(), benchSQL)
		if err != nil {
			b.Fatal(err)
		}
		_ = buf
	}
}

// BenchmarkCompare_MapMarshal models the worst-but-common pattern: scan
// each row into a map[string]any and json.Marshal. We materialise the
// equivalent map by post-processing pg2json's NDJSON output back into
// generic values — which is exactly the round trip a "scan + marshal"
// path would do internally.
func BenchmarkCompare_MapMarshal(b *testing.B) {
	c := openClient(b)
	defer c.Close()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf, err := c.QueryJSON(context.Background(), benchSQL)
		if err != nil {
			b.Fatal(err)
		}
		var v []map[string]any
		if err := json.Unmarshal(buf, &v); err != nil {
			b.Fatal(err)
		}
		out, err := json.Marshal(v)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}
