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
