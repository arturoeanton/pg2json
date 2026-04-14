package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
)

// TestOpenRespectsCtxDeadline verifies that a context with a tiny deadline
// causes Open to return quickly instead of hanging during the post-dial
// startup exchange. A deadline of 1ns is effectively already-expired.
func TestOpenRespectsCtxDeadline(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	start := time.Now()
	c, err := pg2json.Open(ctx, cfg)
	if err == nil {
		c.Close()
		t.Fatal("expected Open to fail under exhausted ctx")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Open took %v; should have honoured ctx promptly", elapsed)
	}
}

// TestOpenSucceedsWithCtx sanity-check that a normal ctx still works
// (i.e. the handshake deadline is cleared before returning).
func TestOpenSucceedsWithCtx(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := pg2json.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A long-ish query against a future point in time must not be aborted
	// by a leaked handshake deadline. pg_sleep(0.3) is comfortably longer
	// than the 5s ctx; if the deadline leaked it would trip.
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("post-open Ping failed (leaked deadline?): %v", err)
	}
}
