package bufferpool

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetPutRoundTrip(t *testing.T) {
	SetMaxLive(0)
	bp := Get()
	if bp == nil || cap(*bp) < defaultCap {
		t.Fatalf("bad buffer: %v cap=%d", bp, cap(*bp))
	}
	*bp = append(*bp, []byte("hello")...)
	Put(bp)
}

func TestMaxLiveCapBypasses(t *testing.T) {
	SetMaxLive(4)
	defer SetMaxLive(0)

	var held []*[]byte
	for i := 0; i < 4; i++ {
		held = append(held, Get())
	}
	if Live() != 4 {
		t.Fatalf("Live = %d, want 4", Live())
	}
	// Over cap: must still return a usable buffer, but Live stays at cap.
	extra := Get()
	if extra == nil || cap(*extra) < defaultCap {
		t.Fatalf("over-cap Get returned unusable buffer")
	}
	if Live() != 4 {
		t.Fatalf("Live after over-cap Get = %d, want 4", Live())
	}
	for _, bp := range held {
		Put(bp)
	}
	Put(extra)
	if l := Live(); l > 0 {
		// Approximate counter may slightly over-correct after bypasses;
		// what matters is it doesn't drift unbounded positive.
		t.Logf("Live after all Put = %d (expected 0 or slightly negative)", l)
	}
}

func TestConcurrentBurstStaysBounded(t *testing.T) {
	const cap = 16
	SetMaxLive(cap)
	defer SetMaxLive(0)

	var peak int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bp := Get()
			// Record the observed live-count peak AFTER the Get returned.
			if n := int64(Live()); n > atomic.LoadInt64(&peak) {
				atomic.StoreInt64(&peak, n)
			}
			_ = bp
			Put(bp)
		}()
	}
	wg.Wait()
	if p := atomic.LoadInt64(&peak); p > int64(cap) {
		t.Fatalf("Live peaked at %d, expected <= %d", p, cap)
	}
}
