// Package wire is the framed reader/writer over a net.Conn. It owns one
// scratch read buffer per connection that is reused across messages; the
// slice returned by ReadMessage is valid only until the next ReadMessage
// call. This is the central allocation-reduction trick of the driver.
package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

const (
	// minRead is the initial scratch capacity. Most rows fit comfortably.
	minRead = 8 * 1024
	// maxMsg caps message size to a sane bound (256 MiB) so a malformed
	// length header cannot make us allocate gigabytes.
	maxMsg = 256 << 20
)

// Conn wraps a net.Conn with a single growable read buffer and a small
// write buffer for building outgoing messages.
//
// Reads go through a bufio.Reader sized at readerBufSize. With narrow
// rows (a single int4 column) each DataRow is ~11–13 bytes over the wire;
// raw net.Conn reads would translate to ~2 syscalls per row. The
// bufio layer amortizes that to one syscall per ~4 KB of wire traffic,
// which is the difference between ~20 MB/s and ~100 MB/s of JSON output
// on small-row queries. The read buffer is still the growable `readBuf`
// below — that's the slice we alias into and return from ReadMessage;
// the bufio just feeds it from userspace, not from the kernel each time.
// DefaultReaderBufSize is the default bufio.Reader size used when the
// caller does not override it via Config.WireReadBufferSize.
const DefaultReaderBufSize = 64 * 1024

type Conn struct {
	nc       net.Conn
	br       *bufio.Reader
	brSize   int
	readBuf  []byte
	writeBuf []byte
	hdr      [5]byte
}

// New wraps nc with the default reader buffer size.
func New(nc net.Conn) *Conn {
	return NewSized(nc, DefaultReaderBufSize)
}

// NewSized wraps nc with a caller-specified bufio size. Sizes smaller
// than 4 KiB are clamped to 4 KiB by the Config layer.
func NewSized(nc net.Conn, bufSize int) *Conn {
	if bufSize <= 0 {
		bufSize = DefaultReaderBufSize
	}
	return &Conn{
		nc:       nc,
		br:       bufio.NewReaderSize(nc, bufSize),
		brSize:   bufSize,
		readBuf:  make([]byte, minRead),
		writeBuf: make([]byte, 0, 1024),
	}
}

func (c *Conn) Net() net.Conn { return c.nc }

// SwapNet replaces the underlying connection (used after the TLS upgrade).
// The scratch buffers are kept.
func (c *Conn) SwapNet(nc net.Conn) {
	c.nc = nc
	// Re-bind the buffered reader to the new (TLS-wrapped) conn. Any
	// pre-buffered bytes from the previous conn are discarded — this is
	// only called immediately after the 'S'/'N' reply byte, before any
	// other reads.
	c.br = bufio.NewReaderSize(nc, c.brSize)
}

// ReadByte reads exactly one byte. Used for the SSLRequest reply, which
// returns 'S' or 'N' without any framing. Reads bypass the bufio layer
// since this happens before any framed traffic.
func (c *Conn) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(c.nc, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (c *Conn) Close() error { return c.nc.Close() }

// ReadMessage reads one v3 message and returns its type byte and the body
// (without the 4-byte length prefix). The returned slice aliases an
// internal buffer and is invalidated by the next ReadMessage call.
func (c *Conn) ReadMessage() (byte, []byte, error) {
	// Zero-copy fast path: when the full message fits inside the bufio
	// buffer we Peek(total), slice into the internal buffer, then
	// Discard to advance. This avoids the per-message memcpy that
	// io.ReadFull would do from bufio's buffer into readBuf. The
	// returned body slice aliases bufio's internal buffer and is
	// valid until the next ReadMessage call — exactly matching the
	// contract already documented for this function.
	hdr, err := c.br.Peek(5)
	if err != nil {
		return 0, nil, err
	}
	t := hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length < 4 {
		return 0, nil, fmt.Errorf("wire: bogus message length %d", length)
	}
	bodyLen := int(length) - 4
	if bodyLen > maxMsg {
		return 0, nil, fmt.Errorf("wire: message too large (%d)", bodyLen)
	}
	total := 5 + bodyLen
	if total <= c.brSize {
		all, err := c.br.Peek(total)
		if err != nil {
			return 0, nil, err
		}
		body := all[5:total]
		if _, err := c.br.Discard(total); err != nil {
			return 0, nil, err
		}
		return t, body, nil
	}
	// Message larger than the bufio buffer. Fall back to ReadFull into
	// readBuf.
	if _, err := c.br.Discard(5); err != nil {
		return 0, nil, err
	}
	if cap(c.readBuf) < bodyLen {
		newCap := cap(c.readBuf) * 2
		if newCap < bodyLen {
			newCap = bodyLen
		}
		c.readBuf = make([]byte, newCap)
	}
	body := c.readBuf[:bodyLen]
	if _, err := io.ReadFull(c.br, body); err != nil {
		return 0, nil, err
	}
	return t, body, nil
}

// ReadStartup reads the *initial* server response which, unusually, has no
// type byte for the very first message in the SSLRequest negotiation. We
// don't currently use it; placeholder for TLS work.
//
// (Phase 2.)

// WriteRaw writes the given bytes verbatim. Used by the startup message
// which has its own framing rules.
func (c *Conn) WriteRaw(p []byte) error {
	for len(p) > 0 {
		n, err := c.nc.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Begin starts building an outgoing message of the given type. It appends
// to the connection's write buffer without truncating prior content, so
// multiple Begin → Finish calls can accumulate into a single TCP write
// (pipelining). The final call should be Send (which flushes) or
// Finish followed by an explicit c.Flush() when the caller wants to send
// several messages at once.
func (c *Conn) Begin(t byte) *Builder {
	start := len(c.writeBuf)
	c.writeBuf = append(c.writeBuf, t)
	c.writeBuf = append(c.writeBuf, 0, 0, 0, 0) // length placeholder
	return &Builder{c: c, startOff: start}
}

// BeginNoType is for the StartupMessage / SSLRequest / CancelRequest
// messages, which omit the type byte.
func (c *Conn) BeginNoType() *Builder {
	start := len(c.writeBuf)
	c.writeBuf = append(c.writeBuf, 0, 0, 0, 0) // length placeholder
	return &Builder{c: c, noType: true, startOff: start}
}

// Flush writes any accumulated (already Finished) messages and resets
// the write buffer. Idempotent on an empty buffer.
func (c *Conn) Flush() error {
	if len(c.writeBuf) == 0 {
		return nil
	}
	err := c.WriteRaw(c.writeBuf)
	c.writeBuf = c.writeBuf[:0]
	return err
}

// Builder is the in-flight outgoing message. All Append* methods write to
// the connection's reusable buffer.
type Builder struct {
	c        *Conn
	noType   bool
	startOff int
}

func (b *Builder) Byte(v byte)       { b.c.writeBuf = append(b.c.writeBuf, v) }
func (b *Builder) Bytes(p []byte)    { b.c.writeBuf = append(b.c.writeBuf, p...) }
func (b *Builder) String(s string)   { b.c.writeBuf = append(b.c.writeBuf, s...) }
func (b *Builder) CString(s string) {
	b.c.writeBuf = append(b.c.writeBuf, s...)
	b.c.writeBuf = append(b.c.writeBuf, 0)
}
func (b *Builder) Int16(v int16) {
	b.c.writeBuf = append(b.c.writeBuf, byte(v>>8), byte(v))
}
func (b *Builder) Int32(v int32) {
	b.c.writeBuf = append(b.c.writeBuf,
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// Finish patches the length prefix of this message in place but does NOT
// write. Use when pipelining several messages before a single c.Flush().
func (b *Builder) Finish() {
	buf := b.c.writeBuf
	start := b.startOff
	if b.noType {
		binary.BigEndian.PutUint32(buf[start:start+4], uint32(len(buf)-start))
	} else {
		binary.BigEndian.PutUint32(buf[start+1:start+5], uint32(len(buf)-start-1))
	}
}

// Send patches the length prefix and flushes the entire write buffer
// to the connection. Equivalent to Finish + c.Flush. Safe to call as
// the terminal action after multiple Begin → Finish pairs; it will
// flush everything accumulated so far.
func (b *Builder) Send() error {
	b.Finish()
	return b.c.Flush()
}

// ErrUnexpected is a sentinel for control-flow asserts.
var ErrUnexpected = errors.New("wire: unexpected message")
