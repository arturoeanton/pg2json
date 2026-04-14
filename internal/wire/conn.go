// Package wire is the framed reader/writer over a net.Conn. It owns one
// scratch read buffer per connection that is reused across messages; the
// slice returned by ReadMessage is valid only until the next ReadMessage
// call. This is the central allocation-reduction trick of the driver.
package wire

import (
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
type Conn struct {
	nc       net.Conn
	readBuf  []byte
	writeBuf []byte
	hdr      [5]byte
}

func New(nc net.Conn) *Conn {
	return &Conn{
		nc:       nc,
		readBuf:  make([]byte, minRead),
		writeBuf: make([]byte, 0, 1024),
	}
}

func (c *Conn) Net() net.Conn { return c.nc }

// SwapNet replaces the underlying connection (used after the TLS upgrade).
// The scratch buffers are kept.
func (c *Conn) SwapNet(nc net.Conn) { c.nc = nc }

// ReadByte reads exactly one byte. Used for the SSLRequest reply, which
// returns 'S' or 'N' without any framing.
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
	if _, err := io.ReadFull(c.nc, c.hdr[:5]); err != nil {
		return 0, nil, err
	}
	t := c.hdr[0]
	length := binary.BigEndian.Uint32(c.hdr[1:5])
	if length < 4 {
		return 0, nil, fmt.Errorf("wire: bogus message length %d", length)
	}
	bodyLen := int(length) - 4
	if bodyLen > maxMsg {
		return 0, nil, fmt.Errorf("wire: message too large (%d)", bodyLen)
	}
	if cap(c.readBuf) < bodyLen {
		// Grow geometrically.
		newCap := cap(c.readBuf) * 2
		if newCap < bodyLen {
			newCap = bodyLen
		}
		c.readBuf = make([]byte, newCap)
	}
	body := c.readBuf[:bodyLen]
	if _, err := io.ReadFull(c.nc, body); err != nil {
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

// Begin starts building an outgoing message of the given type. It returns
// a Builder that the caller appends to and finishes with Send.
func (c *Conn) Begin(t byte) *Builder {
	c.writeBuf = c.writeBuf[:0]
	c.writeBuf = append(c.writeBuf, t)
	c.writeBuf = append(c.writeBuf, 0, 0, 0, 0) // length placeholder
	return &Builder{c: c}
}

// BeginNoType is for the StartupMessage / SSLRequest / CancelRequest
// messages, which omit the type byte.
func (c *Conn) BeginNoType() *Builder {
	c.writeBuf = c.writeBuf[:0]
	c.writeBuf = append(c.writeBuf, 0, 0, 0, 0) // length placeholder
	return &Builder{c: c, noType: true}
}

// Builder is the in-flight outgoing message. All Append* methods write to
// the connection's reusable buffer.
type Builder struct {
	c      *Conn
	noType bool
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

// Send patches the length prefix and writes the message to the connection.
func (b *Builder) Send() error {
	buf := b.c.writeBuf
	if b.noType {
		binary.BigEndian.PutUint32(buf[0:4], uint32(len(buf)))
	} else {
		binary.BigEndian.PutUint32(buf[1:5], uint32(len(buf)-1))
	}
	return b.c.WriteRaw(buf)
}

// ErrUnexpected is a sentinel for control-flow asserts.
var ErrUnexpected = errors.New("wire: unexpected message")
