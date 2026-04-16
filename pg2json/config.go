package pg2json

import (
	"crypto/tls"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SSLMode controls how the client negotiates TLS during connection.
type SSLMode int

const (
	SSLDisable    SSLMode = iota // never use TLS
	SSLPrefer                    // try TLS, fall back to plaintext if server refuses
	SSLRequire                   // demand TLS, no fallback; skip cert verification
	SSLVerifyCA                  // require TLS + valid cert chain (no hostname check)
	SSLVerifyFull                // require TLS + cert chain + hostname match
)

// Config is the connection configuration. Fields are intentionally minimal;
// add as needed.
type Config struct {
	Host           string
	Port           int
	Database       string
	User           string
	Password       string
	ApplicationName string

	DialTimeout time.Duration
	// FlushBytes is the streaming buffer threshold. Default 32 KiB.
	FlushBytes int
	// RuntimeParams is sent during startup (e.g. "search_path", "timezone").
	RuntimeParams map[string]string

	// SSLMode controls TLS negotiation. Default: SSLPrefer.
	SSLMode SSLMode
	// StmtCacheSize bounds the per-connection prepared-statement cache.
	// Default 64. Set to a negative value to disable the cache (fresh
	// Parse on every call).
	StmtCacheSize int
	// TLSConfig overrides the TLS config we would otherwise build from
	// SSLMode + Host. Set this if you need custom CA pools, client certs,
	// SNI, ALPN, etc. Mutually exclusive with SSLMode in practice — if
	// non-nil, SSLMode only decides whether TLS is required vs preferred.
	TLSConfig *tls.Config

	// MaxResponseBytes aborts a query whose JSON output crosses this many
	// bytes. The driver issues a CancelRequest and returns
	// *ResponseTooLargeError. 0 = unlimited (default). Counted across
	// already-flushed bytes plus the in-memory tail, so the bound is on
	// the total response, not the per-flush chunk.
	MaxResponseBytes int64
	// MaxResponseRows aborts a query whose row count crosses this value.
	// Same abort semantics as MaxResponseBytes. 0 = unlimited (default).
	MaxResponseRows int

	// SlowQueryThreshold triggers Observer.OnQuerySlow in addition to
	// OnQueryEnd when a query's total duration meets or exceeds it.
	// 0 = disabled (default).
	SlowQueryThreshold time.Duration

	// FlushInterval bounds the time the streaming buffer may sit without
	// being flushed downstream. When > 0, the driver flushes the pending
	// buffer if this much time elapsed since the last flush, even if the
	// byte threshold was not reached. Useful for Citus queries whose
	// first-byte latency is high and whose consumers (HTTP responses,
	// SSE, NDJSON clients) need steady byte delivery. 0 = disabled.
	FlushInterval time.Duration

	// Keepalive, if > 0, enables TCP keepalive on dialed connections with
	// this interval. Default 30s. Idles behind PgBouncer / NAT die
	// silently without this on long-running sessions.
	Keepalive time.Duration

	// RetryOnSerialization, if true, transparently retries queries that
	// fail with SQLSTATE class 40 (serialization_failure, deadlock_detected,
	// transaction_rollback) when no bytes have been flushed downstream.
	// Max 3 attempts total, exponential backoff starting at 10ms. Default
	// false. Citus shard rebalance surfaces 40001 routinely; enable in
	// environments that run rebalance in production.
	RetryOnSerialization bool

	// DefaultQueryTimeout is a convenience default applied when a caller
	// passes a context with no deadline. The driver derives
	// context.WithTimeout(ctx, DefaultQueryTimeout) and the existing
	// cancel watcher turns an elapsed timeout into a real server-side
	// CancelRequest. 0 = no default (the caller's ctx is used as-is).
	//
	// This is a *sane default*, not a hard security boundary. The hard
	// ceiling belongs in postgresql.conf (statement_timeout). Layers:
	//
	//   1. postgresql.conf statement_timeout  — DBA-owned, final say.
	//   2. Config.DefaultQueryTimeout          — gateway default.
	//   3. ctx.WithTimeout in the HTTP handler — per-request override.
	DefaultQueryTimeout time.Duration

	// WireReadBufferSize is the size of the bufio.Reader fronting the
	// network connection. Messages whose total length (5-byte header +
	// body) fits inside this buffer take a zero-copy fast path via Peek;
	// larger messages fall back to a copy through an internal scratch
	// slice. Default 128 KiB, which covers essentially all DataRow /
	// RowDescription / auth messages. Raise only if you know your
	// workload returns cells larger than 128 KiB that would otherwise
	// trigger the copy fallback on every row. Minimum 4 KiB enforced.
	WireReadBufferSize int

	// TCPRecvBuffer, if > 0, sets SO_RCVBUF on the dialed TCP socket via
	// net.TCPConn.SetReadBuffer. On LAN with non-trivial RTT and wide
	// rows this is the single biggest wire-level knob: the default kernel
	// receive window saturates at a few hundred KB in practice, and the
	// server stalls on ACK. Typical useful values: 1–4 MiB. 0 leaves the
	// kernel default. Ignored for non-TCP transports.
	TCPRecvBuffer int
	// TCPSendBuffer is the matching SO_SNDBUF knob for outbound traffic.
	// Rarely useful for this driver (we send small Bind/Execute/Sync
	// bursts), but exposed for symmetry. 0 = kernel default.
	TCPSendBuffer int

	// RowsHint, if > 0, tells the driver roughly how many rows the query
	// will return. After the first DataRow is encoded the driver grows
	// the output buffer once to `firstRowBytes * RowsHint + slack`,
	// avoiding the repeated doubling-and-copy cost of append() on large
	// result sets. Buffered (QueryJSON) mode is where this pays off;
	// streaming mode caps growth at ~2× FlushBytes because anything
	// above the flush threshold just delays the flush. 0 = disabled.
	RowsHint int
}

func (c *Config) applyDefaults() {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 5432
	}
	if c.User == "" {
		c.User = "postgres"
	}
	if c.Database == "" {
		c.Database = c.User
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.FlushBytes <= 0 {
		c.FlushBytes = 32 * 1024
	}
	if c.ApplicationName == "" {
		c.ApplicationName = "pg2json"
	}
	if c.Keepalive == 0 {
		c.Keepalive = 30 * time.Second
	}
	if c.WireReadBufferSize <= 0 {
		c.WireReadBufferSize = 128 * 1024
	} else if c.WireReadBufferSize < 4*1024 {
		c.WireReadBufferSize = 4 * 1024
	}
	// SSLMode default is "prefer" — match libpq's historical default. The
	// connection will silently fall back to plaintext if the server says
	// no, which is what most local-dev setups expect.
}

// buildTLS returns a *tls.Config to use for the handshake, or nil if TLS
// is disabled.
func (c *Config) buildTLS() *tls.Config {
	if c.SSLMode == SSLDisable {
		return nil
	}
	if c.TLSConfig != nil {
		out := c.TLSConfig.Clone()
		if out.ServerName == "" {
			out.ServerName = c.Host
		}
		return out
	}
	out := &tls.Config{ServerName: c.Host}
	switch c.SSLMode {
	case SSLPrefer, SSLRequire:
		out.InsecureSkipVerify = true
	case SSLVerifyCA:
		// We can't easily skip *only* hostname verification without a
		// custom VerifyConnection callback. Approximate it by setting
		// InsecureSkipVerify and verifying the chain manually.
		out.InsecureSkipVerify = true
		out.VerifyConnection = verifyChainNoHost
	case SSLVerifyFull:
		// stdlib defaults verify chain + hostname.
	}
	return out
}

func verifyChainNoHost(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errNoTLSChain
	}
	opts := tlsVerifyOptionsNoHost(cs)
	_, err := cs.PeerCertificates[0].Verify(opts)
	return err
}

// ParseDSN parses a libpq-style URL: postgres://user:pass@host:port/dbname?param=val
// Only enough is supported to be useful for tests and the demo binary.
func ParseDSN(dsn string) (Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Host: u.Hostname()}
	if p := u.Port(); p != "" {
		cfg.Port, _ = strconv.Atoi(p)
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Password = pw
		}
	}
	if path := strings.TrimPrefix(u.Path, "/"); path != "" {
		cfg.Database = path
	}
	q := u.Query()
	if app := q.Get("application_name"); app != "" {
		cfg.ApplicationName = app
	}
	if v := q.Get("tcp_recv_buffer"); v != "" {
		cfg.TCPRecvBuffer, _ = strconv.Atoi(v)
	}
	if v := q.Get("tcp_send_buffer"); v != "" {
		cfg.TCPSendBuffer, _ = strconv.Atoi(v)
	}
	if v := q.Get("wire_read_buffer"); v != "" {
		cfg.WireReadBufferSize, _ = strconv.Atoi(v)
	}
	switch q.Get("sslmode") {
	case "disable":
		cfg.SSLMode = SSLDisable
	case "", "prefer":
		cfg.SSLMode = SSLPrefer
	case "require":
		cfg.SSLMode = SSLRequire
	case "verify-ca":
		cfg.SSLMode = SSLVerifyCA
	case "verify-full":
		cfg.SSLMode = SSLVerifyFull
	}
	cfg.applyDefaults()
	return cfg, nil
}
