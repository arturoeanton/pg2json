package pg2json

import (
	"bytes"
	"testing"
	"time"
)

// TestFlushByByteThreshold: classic size-based flush trips after buffer
// crosses threshold.
func TestFlushByByteThreshold(t *testing.T) {
	var out bytes.Buffer
	f := &flushingWriter{w: &out, threshold: 16, lastFlush: time.Now()}
	_, _ = f.Write([]byte("12345678"))
	if err := f.MaybeFlush(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("should not have flushed yet, got %d", out.Len())
	}
	_, _ = f.Write([]byte("abcdefghij"))
	if err := f.MaybeFlush(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 18 {
		t.Fatalf("expected flush of 18 bytes, got %d", out.Len())
	}
}

// TestFlushByInterval: time-based flush trips even when the buffer is
// below the byte threshold.
func TestFlushByInterval(t *testing.T) {
	var out bytes.Buffer
	f := &flushingWriter{
		w:         &out,
		threshold: 1 << 20, // effectively unreachable
		interval:  20 * time.Millisecond,
		lastFlush: time.Now(),
	}
	_, _ = f.Write([]byte("hello"))
	if err := f.MaybeFlush(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("should not have flushed before interval, got %d", out.Len())
	}
	time.Sleep(30 * time.Millisecond)
	if err := f.MaybeFlush(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 5 {
		t.Fatalf("expected interval-triggered flush, got %d", out.Len())
	}
}

// TestFlushIntervalDisabled: interval=0 leaves everything driven by bytes
// only; a tiny buffer never flushes.
func TestFlushIntervalDisabled(t *testing.T) {
	var out bytes.Buffer
	f := &flushingWriter{w: &out, threshold: 1 << 20, interval: 0, lastFlush: time.Now()}
	_, _ = f.Write([]byte("hello"))
	time.Sleep(20 * time.Millisecond)
	if err := f.MaybeFlush(); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("interval=0 must not flush by time, got %d", out.Len())
	}
}

// TestFlushIntervalEmptyBufferNoop: interval elapsed but buffer is empty,
// don't do an empty write to downstream.
func TestFlushIntervalEmptyBufferNoop(t *testing.T) {
	var out countWriter
	f := &flushingWriter{
		w:         &out,
		threshold: 1024,
		interval:  5 * time.Millisecond,
		lastFlush: time.Now(),
	}
	time.Sleep(15 * time.Millisecond)
	if err := f.MaybeFlush(); err != nil {
		t.Fatal(err)
	}
	if out.writes != 0 {
		t.Fatalf("expected no downstream writes for empty buffer, got %d", out.writes)
	}
}

type countWriter struct {
	writes int
	bytes  int
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.writes++
	c.bytes += len(p)
	return len(p), nil
}
