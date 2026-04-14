package pg2json

import (
	"io"
	"time"

	"github.com/arturoeanton/pg2json/internal/bufferpool"
)

// outWriter abstracts the two output paths: an in-memory buffer (for
// QueryJSON) and a flushing writer wrapping an io.Writer (for the streaming
// API). The encoder loop appends into Buf() then calls SetBuf() so it can
// keep the slice header local and let the compiler see the appends.
//
// Committed/Reset support the retry path. Once any byte has been delivered
// to the user, Committed() returns true and Reset() is a no-op. As long as
// nothing has been flushed, Reset() discards the in-memory buffer so the
// caller can re-run the query from scratch (e.g. after a re-prepare on
// PgBouncer connection rotation).
type outWriter interface {
	Buf() []byte
	SetBuf([]byte)
	Write([]byte) (int, error)
	WriteByte(byte) error
	MaybeFlush() error
	Flush() error
	Committed() bool
	Reset()
	BytesOut() int
}

// bufWriter is the QueryJSON path: append into a pooled []byte and never
// flush. Flush() is a no-op.
type bufWriter struct {
	p *[]byte
}

func (b *bufWriter) Buf() []byte                  { return *b.p }
func (b *bufWriter) SetBuf(p []byte)              { *b.p = p }
func (b *bufWriter) Write(p []byte) (int, error)  { *b.p = append(*b.p, p...); return len(p), nil }
func (b *bufWriter) WriteByte(c byte) error       { *b.p = append(*b.p, c); return nil }
func (b *bufWriter) MaybeFlush() error            { return nil }
func (b *bufWriter) Flush() error                 { return nil }
func (b *bufWriter) Committed() bool              { return false } // never until QueryJSON returns
func (b *bufWriter) Reset()                       { *b.p = (*b.p)[:0] }
func (b *bufWriter) BytesOut() int                { return len(*b.p) }

// flushingWriter accumulates into a buffer and flushes to the underlying
// writer once the buffer crosses `threshold` OR `interval` has elapsed
// since the last flush (if interval > 0). Because flushes happen between
// row appends, we never split a row across writes — important for NDJSON
// consumers that scan line-by-line.
type flushingWriter struct {
	w         io.Writer
	threshold int
	interval  time.Duration
	lastFlush time.Time
	buf       []byte
	src       *[]byte // pool source so we can return it on close
	committed bool    // any flush has happened
	written   int     // total bytes flushed downstream
}

func (f *flushingWriter) Buf() []byte     { return f.buf }
func (f *flushingWriter) SetBuf(p []byte) { f.buf = p }

func (f *flushingWriter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	return len(p), nil
}

func (f *flushingWriter) WriteByte(c byte) error {
	f.buf = append(f.buf, c)
	return nil
}

func (f *flushingWriter) MaybeFlush() error {
	if len(f.buf) >= f.threshold {
		return f.Flush()
	}
	if f.interval > 0 && len(f.buf) > 0 && time.Since(f.lastFlush) >= f.interval {
		return f.Flush()
	}
	return nil
}

func (f *flushingWriter) Flush() error {
	if len(f.buf) > 0 {
		if _, err := f.w.Write(f.buf); err != nil {
			f.releaseBuf()
			return err
		}
		f.committed = true
		f.written += len(f.buf)
		f.buf = f.buf[:0]
	}
	if f.interval > 0 {
		f.lastFlush = time.Now()
	}
	return nil
}

func (f *flushingWriter) Committed() bool { return f.committed }
func (f *flushingWriter) BytesOut() int   { return f.written + len(f.buf) }

func (f *flushingWriter) Reset() {
	if f.committed {
		return
	}
	f.buf = f.buf[:0]
}

func (f *flushingWriter) releaseBuf() {
	if f.src != nil {
		*f.src = f.buf[:0]
		bufferpool.Put(f.src)
		f.src = nil
	}
}
