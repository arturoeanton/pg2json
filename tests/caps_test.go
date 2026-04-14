package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/arturoeanton/pg2json/pg2json"
)

func openWithCfg(t testing.TB, mut func(*pg2json.Config)) *pg2json.Client {
	t.Helper()
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set; skipping integration test")
	}
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(&cfg)
	}
	c, err := pg2json.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMaxResponseRowsBuffered(t *testing.T) {
	c := openWithCfg(t, func(cfg *pg2json.Config) { cfg.MaxResponseRows = 10 })
	defer c.Close()

	_, err := c.QueryJSON(context.Background(),
		"SELECT g FROM generate_series(1, 1000) g")
	if err == nil {
		t.Fatal("expected cap error")
	}
	var ce *pg2json.ResponseTooLargeError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ResponseTooLargeError, got %T: %v", err, err)
	}
	if !errors.Is(err, pg2json.ErrResponseTooLarge) {
		t.Fatalf("errors.Is(ErrResponseTooLarge) = false")
	}
	if ce.Limit != "rows" || ce.LimitVal != 10 || ce.Observed < 10 {
		t.Fatalf("unexpected cap details: %+v", ce)
	}
	if ce.Committed {
		t.Fatalf("buffered path must report Committed=false, got %+v", ce)
	}
}

func TestMaxResponseBytesBuffered(t *testing.T) {
	c := openWithCfg(t, func(cfg *pg2json.Config) { cfg.MaxResponseBytes = 1024 })
	defer c.Close()

	_, err := c.QueryJSON(context.Background(),
		"SELECT g, repeat('x', 100) AS pad FROM generate_series(1, 10000) g")
	if err == nil {
		t.Fatal("expected cap error")
	}
	var ce *pg2json.ResponseTooLargeError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ResponseTooLargeError, got %T: %v", err, err)
	}
	if ce.Limit != "bytes" || ce.LimitVal != 1024 || ce.Observed < 1024 {
		t.Fatalf("unexpected cap details: %+v", ce)
	}
}

func TestMaxResponseBytesStreamingCommitted(t *testing.T) {
	// FlushBytes well below the cap so flushes happen before the cap trips.
	c := openWithCfg(t, func(cfg *pg2json.Config) {
		cfg.MaxResponseBytes = 8 * 1024
		cfg.FlushBytes = 256
	})
	defer c.Close()

	var buf bytes.Buffer
	err := c.StreamNDJSON(context.Background(), &buf,
		"SELECT g, repeat('y', 100) AS pad FROM generate_series(1, 10000) g")
	if err == nil {
		t.Fatal("expected cap error")
	}
	var ce *pg2json.ResponseTooLargeError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ResponseTooLargeError, got %T: %v", err, err)
	}
	if !ce.Committed {
		t.Fatalf("streaming with FlushBytes < cap should report Committed=true, got %+v", ce)
	}
	if buf.Len() == 0 {
		t.Fatal("expected some bytes flushed downstream before cap")
	}
}

func TestConnReusableAfterCap(t *testing.T) {
	c := openWithCfg(t, func(cfg *pg2json.Config) { cfg.MaxResponseRows = 5 })
	defer c.Close()

	// Trip the cap.
	if err := c.StreamNDJSON(context.Background(), io.Discard,
		"SELECT g FROM generate_series(1, 1000) g"); err == nil {
		t.Fatal("expected cap error")
	}
	// Connection should be drained back to RFQ; next query must succeed.
	buf, err := c.QueryJSON(context.Background(), "SELECT 1::int AS a")
	if err != nil {
		t.Fatalf("conn unusable after cap abort: %v", err)
	}
	if string(buf) != `[{"a":1}]` {
		t.Fatalf("got %s", buf)
	}
}

func TestCapsDefaultUnlimited(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	// No caps set: a moderately large query must complete normally.
	if err := c.StreamNDJSON(context.Background(), io.Discard,
		"SELECT g FROM generate_series(1, 50000) g"); err != nil {
		t.Fatalf("unexpected error with caps unset: %v", err)
	}
}
