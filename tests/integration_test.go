// Integration tests against a real PostgreSQL. Skipped unless
// PG2JSON_TEST_DSN is set.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
)

func openClient(t testing.TB) *pg2json.Client {
	t.Helper()
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set; skipping integration test")
	}
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pg2json.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSimpleSelect(t *testing.T) {
	c := openClient(t)
	defer c.Close()

	buf, err := c.QueryJSON(context.Background(),
		"SELECT 1::int AS a, 'hi' AS b, NULL::text AS c")
	if err != nil {
		t.Fatal(err)
	}
	var v []map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatalf("invalid JSON %s: %v", buf, err)
	}
	if len(v) != 1 || v[0]["a"].(float64) != 1 || v[0]["b"] != "hi" || v[0]["c"] != nil {
		t.Fatalf("bad row: %v", v)
	}
}

func TestRejectNonSelect(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	_, err := c.QueryJSON(context.Background(), "INSERT INTO foo VALUES (1)")
	if err == nil {
		t.Fatal("expected rejection")
	}
}

func TestParameterized(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	buf, err := c.QueryJSON(context.Background(),
		"SELECT $1::int AS x, $2::text AS y", 42, "world")
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"x":42,"y":"world"}]`
	if string(buf) != want {
		t.Fatalf("got %s want %s", buf, want)
	}
}

func TestNDJSONStream(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	var out bytes.Buffer
	if err := c.StreamNDJSON(context.Background(), &out,
		"SELECT g AS i FROM generate_series(1, 5) g"); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte{'\n'})
	if len(lines) != 5 {
		t.Fatalf("got %d lines", len(lines))
	}
	for _, l := range lines {
		var v map[string]any
		if err := json.Unmarshal(l, &v); err != nil {
			t.Fatalf("bad line %s: %v", l, err)
		}
	}
}

func TestContextCancel(t *testing.T) {
	c := openClient(t)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// pg_sleep(5) on the server. We expect the context timeout to fire
	// CancelRequest and tear the read down promptly.
	err := c.StreamNDJSON(ctx, io.Discard, "SELECT pg_sleep(5)")
	if err == nil {
		t.Fatal("expected error from cancelled query")
	}
}

func BenchmarkStreamMixed(b *testing.B) {
	c := openClient(b)
	defer c.Close()
	sql := `SELECT g AS id, 'row-'||g AS name, (g::float8) AS score,
	         (g%2=0) AS flag FROM generate_series(1, 1000) g`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := c.StreamNDJSON(context.Background(), io.Discard, sql); err != nil {
			b.Fatal(err)
		}
	}
}
