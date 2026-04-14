package pg2json

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arturoeanton/pg2json/internal/auth"
	"github.com/arturoeanton/pg2json/internal/pgerr"
	"github.com/arturoeanton/pg2json/internal/protocol"
	"github.com/arturoeanton/pg2json/internal/rows"
	"github.com/arturoeanton/pg2json/internal/wire"
)

// sslRequestCode is the magic version number for the SSLRequest message
// (PG_PROTOCOL(1234,5679) in libpq). See pqcomm.h.
const sslRequestCode int32 = 80877103

// cancelRequestCode is the magic for CancelRequest (PG_PROTOCOL(1234,5678)).
const cancelRequestCode int32 = 80877102

// Client is a single PostgreSQL connection specialised for SELECT-to-JSON.
// It is not safe for concurrent use; wrap it in a pool if you need that.
type Client struct {
	cfg    Config
	conn   *wire.Conn
	params map[string]string
	pid    int32
	skey   int32

	planCache map[string]*rows.Plan
	mu        sync.Mutex // guards planCache

	stmts *stmtCache

	scram *auth.SCRAM

	observer    Observer
	cnt         counters
	lastRows    int
	lastRetries int
}

// Open dials the server and completes the startup handshake. The full
// context (including any deadline) covers the dial, TLS negotiation, and
// the startup message exchange — not just the TCP connect.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()

	d := net.Dialer{
		Timeout:   cfg.DialTimeout,
		KeepAlive: cfg.Keepalive, // negative disables; 0 means OS default
	}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("pg2json: dial: %w", err)
	}
	// Push the ctx deadline (or DialTimeout, if ctx has none) into the
	// socket for the duration of the handshake so reads during TLS / auth
	// honour cancellation. Cleared before we hand the Client out.
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	} else if cfg.DialTimeout > 0 {
		_ = nc.SetDeadline(time.Now().Add(cfg.DialTimeout))
	}
	c := &Client{
		cfg:       cfg,
		conn:      wire.New(nc),
		params:    make(map[string]string, 16),
		planCache: make(map[string]*rows.Plan, 16),
		stmts:     newStmtCache(cfg.StmtCacheSize),
		observer:  noopObserver{},
	}
	if err := c.maybeNegotiateTLS(); err != nil {
		_ = c.conn.Close()
		return nil, err
	}
	if err := c.startup(); err != nil {
		_ = c.conn.Close()
		return nil, err
	}
	// Handshake done; clear the deadline so subsequent query reads are
	// governed only by per-query context.
	_ = c.conn.Net().SetDeadline(time.Time{})
	return c, nil
}

// maybeNegotiateTLS sends SSLRequest, reads the 1-byte reply, and either
// upgrades the connection or — if SSLMode allows it — falls back to
// plaintext.
func (c *Client) maybeNegotiateTLS() error {
	tlsCfg := c.cfg.buildTLS()
	if tlsCfg == nil {
		return nil
	}
	b := c.conn.BeginNoType()
	b.Int32(sslRequestCode)
	if err := b.Send(); err != nil {
		return fmt.Errorf("pg2json: SSLRequest: %w", err)
	}
	resp, err := c.conn.ReadByte()
	if err != nil {
		return fmt.Errorf("pg2json: SSLRequest reply: %w", err)
	}
	switch resp {
	case 'S':
		t := tls.Client(c.conn.Net(), tlsCfg)
		if err := t.Handshake(); err != nil {
			return fmt.Errorf("pg2json: TLS handshake: %w", err)
		}
		c.conn.SwapNet(t)
		return nil
	case 'N':
		if c.cfg.SSLMode >= SSLRequire {
			return fmt.Errorf("pg2json: server refused TLS but ssl mode is require/verify-*")
		}
		return nil
	case 'E':
		// Server sent an ErrorResponse instead. Read it to surface the
		// real reason (e.g. "unsupported frontend protocol").
		body, _, err := readErrorBody(c.conn.Net())
		if err != nil {
			return fmt.Errorf("pg2json: SSLRequest error reply: %w", err)
		}
		return pgerr.Parse(body)
	default:
		return fmt.Errorf("pg2json: unexpected SSLRequest reply byte 0x%02x", resp)
	}
}

// readErrorBody reads a length-prefixed message body from a raw net.Conn
// (used during the SSL negotiation where we already consumed the type byte).
func readErrorBody(nc net.Conn) ([]byte, byte, error) {
	var hdr [4]byte
	if _, err := nc.Read(hdr[:]); err != nil {
		return nil, 0, err
	}
	length := int(uint32(hdr[0])<<24 | uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3]))
	if length < 4 {
		return nil, 0, fmt.Errorf("pg2json: bogus error length %d", length)
	}
	body := make([]byte, length-4)
	if _, err := nc.Read(body); err != nil {
		return nil, 0, err
	}
	return body, 'E', nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Ping issues an empty simple-query and waits for ReadyForQuery. It is a
// cheap liveness check used by the pool after long idle periods (helps
// catch connections silently killed by PgBouncer's server_idle_timeout,
// firewall NAT timeouts, etc.). The cost is one round-trip.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.attachContext(ctx); err != nil {
		return err
	}
	defer c.detachContext()

	b := c.conn.Begin(protocol.MsgQuery)
	b.CString("")
	if err := b.Send(); err != nil {
		return err
	}
	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		switch t {
		case protocol.MsgEmptyQueryResponse, protocol.MsgCommandComplete:
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgErrorResponse:
			return pgerr.Parse(body)
		case protocol.MsgReadyForQuery:
			return nil
		default:
			return fmt.Errorf("pg2json: unexpected msg %q during ping", t)
		}
	}
}

// SendCancel opens a brand-new TCP connection to the same server and
// sends a CancelRequest carrying this connection's BackendKeyData. The
// server cancels any in-flight query on the original connection. The
// side connection is closed immediately. Errors are best-effort: if the
// cancel fails, the caller's context-cancellation path will still take
// effect via the deadline mechanism.
func (c *Client) SendCancel(ctx context.Context) error {
	if c.pid == 0 {
		return fmt.Errorf("pg2json: no backend key data; cannot cancel")
	}
	d := net.Dialer{Timeout: c.cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port)))
	if err != nil {
		return err
	}
	defer nc.Close()
	// 16 bytes: length=16, code, pid, secret.
	var buf [16]byte
	beU32put(buf[0:4], 16)
	beU32put(buf[4:8], uint32(cancelRequestCode))
	beU32put(buf[8:12], uint32(c.pid))
	beU32put(buf[12:16], uint32(c.skey))
	_, err = nc.Write(buf[:])
	return err
}

func beU32put(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// ParameterStatus returns the most recent value the server sent for the
// given runtime parameter (e.g. "server_version", "client_encoding").
func (c *Client) ParameterStatus(name string) string { return c.params[name] }

func (c *Client) startup() error {
	b := c.conn.BeginNoType()
	b.Int32(protocol.ProtocolVersion3)
	b.CString("user")
	b.CString(c.cfg.User)
	if c.cfg.Database != "" {
		b.CString("database")
		b.CString(c.cfg.Database)
	}
	b.CString("application_name")
	b.CString(c.cfg.ApplicationName)
	// Ask the server to give us text format / UTF-8.
	b.CString("client_encoding")
	b.CString("UTF8")
	for k, v := range c.cfg.RuntimeParams {
		b.CString(k)
		b.CString(v)
	}
	b.Byte(0) // terminating empty key
	if err := b.Send(); err != nil {
		return fmt.Errorf("pg2json: send startup: %w", err)
	}

	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("pg2json: read startup reply: %w", err)
		}
		switch t {
		case protocol.MsgAuthentication:
			if err := c.handleAuth(body); err != nil {
				return err
			}
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgBackendKeyData:
			if len(body) >= 8 {
				c.pid = int32(beU32(body[0:4]))
				c.skey = int32(beU32(body[4:8]))
			}
		case protocol.MsgReadyForQuery:
			return nil
		case protocol.MsgErrorResponse:
			return pgerr.Parse(body)
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgNegotiateProtocol:
			// We asked for 3.0; servers that speak 3.x will negotiate
			// down silently. Accept and continue.
		default:
			return fmt.Errorf("pg2json: unexpected startup msg %q", t)
		}
	}
}

func (c *Client) handleAuth(body []byte) error {
	if len(body) < 4 {
		return fmt.Errorf("pg2json: short Authentication msg")
	}
	code := int32(beU32(body[:4]))
	switch code {
	case protocol.AuthOK:
		return nil
	case protocol.AuthCleartextPassword:
		b := c.conn.Begin(protocol.MsgPassword)
		b.CString(c.cfg.Password)
		return b.Send()
	case protocol.AuthMD5Password:
		if len(body) < 8 {
			return fmt.Errorf("pg2json: short MD5 salt")
		}
		salt := body[4:8]
		hashed := auth.MD5Password(c.cfg.User, c.cfg.Password, salt)
		b := c.conn.Begin(protocol.MsgPassword)
		b.CString(hashed)
		return b.Send()
	case protocol.AuthSASL:
		mech, err := auth.PickMechanism(body[4:])
		if err != nil {
			return err
		}
		s, err := auth.NewSCRAM(c.cfg.Password)
		if err != nil {
			return err
		}
		c.scram = s
		first := s.ClientFirst()
		b := c.conn.Begin(protocol.MsgPassword)
		b.CString(mech)
		b.Int32(int32(len(first)))
		b.Bytes(first)
		return b.Send()
	case protocol.AuthSASLContinue:
		if c.scram == nil {
			return fmt.Errorf("pg2json: SASLContinue without SASL init")
		}
		final, err := c.scram.ClientFinal(body[4:])
		if err != nil {
			return err
		}
		b := c.conn.Begin(protocol.MsgPassword)
		b.Bytes(final)
		return b.Send()
	case protocol.AuthSASLFinal:
		if c.scram == nil {
			return fmt.Errorf("pg2json: SASLFinal without SASL init")
		}
		return c.scram.Verify(body[4:])
	default:
		return fmt.Errorf("pg2json: unsupported auth code %d", code)
	}
}

// splitParameter parses a ParameterStatus body: cstring name, cstring value.
func splitParameter(b []byte) (string, string) {
	for i, c := range b {
		if c == 0 {
			name := string(b[:i])
			rest := b[i+1:]
			for j, c2 := range rest {
				if c2 == 0 {
					return name, string(rest[:j])
				}
			}
			return name, ""
		}
	}
	return "", ""
}

func beU32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// rejectNonSelect is a tiny guard. We accept SELECT, WITH (CTE → SELECT),
// VALUES, TABLE, SHOW. Nothing else. Comments and whitespace at the start
// are stripped.
func rejectNonSelect(sql string) error {
	s := strings.TrimLeftFunc(sql, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	// Strip leading "--..." line comments.
	for strings.HasPrefix(s, "--") {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = strings.TrimLeftFunc(s[idx+1:], func(r rune) bool {
				return r == ' ' || r == '\t' || r == '\n' || r == '\r'
			})
		} else {
			s = ""
			break
		}
	}
	// Strip leading /* ... */ block comments.
	for strings.HasPrefix(s, "/*") {
		end := strings.Index(s, "*/")
		if end < 0 {
			s = ""
			break
		}
		s = strings.TrimLeftFunc(s[end+2:], func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})
	}
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "SELECT"),
		strings.HasPrefix(upper, "WITH"),
		strings.HasPrefix(upper, "VALUES"),
		strings.HasPrefix(upper, "TABLE"),
		strings.HasPrefix(upper, "SHOW"):
		return nil
	}
	return fmt.Errorf("pg2json: only SELECT-shaped queries are accepted")
}
