package tests

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/internal/pgerr"
	"github.com/arturoeanton/pg2json/pg2json"
)

// TestOnNoticeReceivesWarning calls pg_advisory_unlock on a lock the
// session does not hold. The server responds with a WARNING that arrives
// on the wire as NoticeResponse (type 'N'), which must reach the observer.
func TestOnNoticeReceivesWarning(t *testing.T) {
	c := openClient(t)
	defer c.Close()

	var mu sync.Mutex
	var got []*pgerr.Error
	c.SetObserver(observerFunc{
		notice: func(n *pgerr.Error) {
			mu.Lock()
			got = append(got, n)
			mu.Unlock()
		},
	})

	if _, err := c.QueryJSON(context.Background(),
		"SELECT pg_advisory_unlock(99999)"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("expected at least one NoticeResponse delivered to observer")
	}
	if got[0].Severity != "WARNING" {
		t.Fatalf("expected WARNING severity, got %q (msg=%q)", got[0].Severity, got[0].Message)
	}
}

func TestOnQuerySlowFires(t *testing.T) {
	c := openWithCfg(t, func(cfg *pg2json.Config) {
		cfg.SlowQueryThreshold = 50 * time.Millisecond
	})
	defer c.Close()

	var hits int64
	var lastDur time.Duration
	c.SetObserver(observerFunc{
		slow: func(e pg2json.QueryEvent) {
			atomic.AddInt64(&hits, 1)
			lastDur = e.Duration
		},
	})

	// pg_sleep(0.1) guarantees > 50ms.
	if err := c.StreamNDJSON(context.Background(), io.Discard,
		"SELECT pg_sleep(0.1)"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("expected OnQuerySlow to fire once, got %d", hits)
	}
	if lastDur < 50*time.Millisecond {
		t.Fatalf("duration should be >= threshold, got %v", lastDur)
	}

	// A fast query must NOT fire OnQuerySlow.
	atomic.StoreInt64(&hits, 0)
	if _, err := c.QueryJSON(context.Background(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if h := atomic.LoadInt64(&hits); h != 0 {
		t.Fatalf("fast query should not fire OnQuerySlow, got %d", h)
	}
}
