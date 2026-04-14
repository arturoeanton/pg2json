// Package bufferpool is a tiny sync.Pool wrapper specialised for []byte
// scratch buffers. We expose Get/Put rather than a typed Buffer so callers
// can resize freely (append) and the pool just stores the largest backing
// array each goroutine has produced.
package bufferpool

import "sync"

const defaultCap = 32 * 1024

var pool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, defaultCap)
		return &b
	},
}

// Get returns a zero-length slice with at least defaultCap capacity.
func Get() *[]byte {
	bp := pool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

// Put returns the buffer to the pool. The caller must not retain a reference.
// Buffers larger than 1 MiB are dropped to avoid pinning memory after a
// burst of large responses.
func Put(bp *[]byte) {
	if bp == nil {
		return
	}
	if cap(*bp) > 1<<20 {
		return
	}
	pool.Put(bp)
}
