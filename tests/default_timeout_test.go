package tests

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
)

func TestDefaultQueryTimeoutFiresCancel(t *testing.T) {
	c := openWithCfg(t, func(cfg *pg2json.Config) {
		cfg.DefaultQueryTimeout = 150 * time.Millisecond
	})
	defer c.Close()

	start := time.Now()
	err := c.StreamNDJSON(context.Background(), io.Discard, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from server-cancelled query")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("DefaultQueryTimeout did not fire; elapsed=%v", elapsed)
	}
}

func TestDefaultQueryTimeoutDoesNotOverrideShorterCtx(t *testing.T) {
	c := openWithCfg(t, func(cfg *pg2json.Config) {
		cfg.DefaultQueryTimeout = 10 * time.Second
	})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.StreamNDJSON(ctx, io.Discard, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("caller's shorter ctx should have won; elapsed=%v", elapsed)
	}
	// The error will be the server-side cancel (57014) or a local ctx
	// error depending on timing — both are acceptable; what matters is
	// the query did not run for 5s.
	_ = errors.Is(err, context.DeadlineExceeded)
}
