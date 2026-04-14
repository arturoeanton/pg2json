package pool

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
)

func openCfg(t testing.TB) Config {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	cfg, err := pg2json.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Config: cfg, MaxConns: 4}
}

func TestAcquireRelease(t *testing.T) {
	p, err := New(openCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for i := 0; i < 8; i++ {
		c, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		buf, err := c.QueryJSON(context.Background(), "SELECT $1::int AS n", i)
		c.Release()
		if err != nil {
			t.Fatal(err)
		}
		if len(buf) == 0 {
			t.Fatal("empty result")
		}
	}
	st := p.Stats()
	if st.Open > 4 || st.Idle == 0 {
		t.Fatalf("bad stats: %+v", st)
	}
}

func TestConcurrentAcquire(t *testing.T) {
	p, err := New(openCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := p.Acquire(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer c.Release()
			if _, err := c.QueryJSON(ctx, "SELECT $1::int AS n", i); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestAcquireTimeoutOnFull(t *testing.T) {
	cfg := openCfg(t)
	cfg.MaxConns = 1
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	hold, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); err == nil {
		t.Fatal("expected timeout")
	}
}

func TestDrainWaitsForInFlight(t *testing.T) {
	p, err := New(openCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	go func() {
		// Hold the conn briefly, then release.
		time.Sleep(80 * time.Millisecond)
		c.Release()
		close(released)
	}()

	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := p.Drain(dctx); err != nil {
		t.Fatalf("Drain returned error: %v", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatalf("Drain returned too fast; did it actually wait for the in-flight conn?")
	}
	<-released
	// New Acquires must be rejected.
	if _, err := p.Acquire(ctx); err == nil {
		t.Fatal("Acquire after Drain should fail")
	}
	_ = p.Close()
}

func TestDrainContextTimeout(t *testing.T) {
	p, err := New(openCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()

	dctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Drain(dctx); err == nil {
		t.Fatal("Drain should have timed out while conn held")
	}
}

func TestWaitIdleAfterRelease(t *testing.T) {
	p, err := New(openCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		c.Release()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle failed: %v", err)
	}
	if st := p.Stats(); st.InUse != 0 {
		t.Fatalf("expected InUse=0, got %+v", st)
	}
}
