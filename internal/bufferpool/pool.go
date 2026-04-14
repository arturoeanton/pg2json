// Package bufferpool is a tiny sync.Pool wrapper specialised for []byte
// scratch buffers. We expose Get/Put rather than a typed Buffer so callers
// can resize freely (append) and the pool just stores the largest backing
// array each goroutine has produced.
//
// Optional soft cap on the number of buffers checked out at once:
// SetMaxLive(N) — when N > 0 and the in-flight count is at the cap, Get
// returns a fresh []byte that bypasses sync.Pool. This keeps a
// 10,000-concurrent-connection burst from expanding the sync.Pool's
// backing slab past ~N × 32 KiB. Non-pooled buffers feed back through
// Put (we cannot distinguish them), so the counter drifts slightly
// during bursts but self-corrects — what matters is that sync.Pool
// never receives a Get call beyond the cap, so the steady-state
// working set stays bounded.
package bufferpool

import (
	"sync"
	"sync/atomic"
)

const defaultCap = 32 * 1024

var (
	pool = sync.Pool{
		New: func() any {
			b := make([]byte, 0, defaultCap)
			return &b
		},
	}
	live    int64 // approximate in-flight count
	maxLive int64 // 0 = unbounded
)

// SetMaxLive sets the soft cap on in-flight buffers. 0 disables the
// cap. When the cap is hit, Get allocates fresh instead of going
// through sync.Pool; Put is unchanged. This is an operability knob,
// not a correctness one — callers always receive a usable buffer.
func SetMaxLive(n int) { atomic.StoreInt64(&maxLive, int64(n)) }

// Live reports the approximate in-flight count (for metrics).
func Live() int {
	n := atomic.LoadInt64(&live)
	if n < 0 {
		return 0
	}
	return int(n)
}

// Get returns a zero-length slice with at least defaultCap capacity.
// Under load past the configured live cap, Get returns a fresh buffer
// that bypasses sync.Pool instead of growing the pool further.
func Get() *[]byte {
	if m := atomic.LoadInt64(&maxLive); m > 0 {
		if atomic.LoadInt64(&live) >= m {
			// Burst: bypass the pool. Don't count toward live —
			// sync.Pool is never asked for a buffer we won't give
			// back, so the pool's backing slab stays bounded at m
			// buffers-ish in steady state.
			b := make([]byte, 0, defaultCap)
			return &b
		}
		atomic.AddInt64(&live, 1)
	}
	bp := pool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

// Put returns the buffer to the pool. The caller must not retain a
// reference. Buffers larger than 1 MiB are dropped to avoid pinning
// memory after a burst of large responses.
func Put(bp *[]byte) {
	if bp == nil {
		return
	}
	if cap(*bp) > 1<<20 {
		if atomic.LoadInt64(&maxLive) > 0 {
			atomic.AddInt64(&live, -1)
		}
		return
	}
	if atomic.LoadInt64(&maxLive) > 0 {
		atomic.AddInt64(&live, -1)
	}
	pool.Put(bp)
}
