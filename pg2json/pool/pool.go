// Package pool is a small, dependency-free connection pool for *pg2json.Client.
//
// Design choices:
//   - Bounded by MaxConns. Acquire() blocks (with context) when full.
//   - Idle conns live on a LIFO stack so the warm ones stay warm.
//   - Background reaper closes conns idle longer than IdleTimeout.
//   - On a query error, the caller releases with Discard() so the broken
//     conn does not return to the pool.
//   - All goroutines exit on Close().
package pool

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/arturoeanton/pg2json/pg2json"
)

// Config configures the pool. Zero values are sane defaults.
type Config struct {
	pg2json.Config

	MaxConns       int           // default 16
	MinIdle        int           // default 0
	IdleTimeout    time.Duration // default 5m
	AcquireTimeout time.Duration // default unbounded; per-call ctx wins
	HealthCheck    time.Duration // background reaper interval; default 30s
	// PingAfterIdle is the threshold above which Acquire pings the
	// idle connection before returning it. Default: 30s. Set to 0 to
	// disable. Helps survive PgBouncer's server_idle_timeout (default
	// 600s) and middlebox NAT timeouts.
	PingAfterIdle time.Duration
	// MaxConnLifetime caps the total wall-clock lifetime of a pooled
	// connection. Past this, Acquire closes and reopens. Defaults to 1h.
	// Bound the long-tail growth of server-side state (prepared stmts,
	// catalog caches, etc.) on long-lived backends.
	MaxConnLifetime time.Duration
}

func (c *Config) applyDefaults() {
	if c.MaxConns <= 0 {
		c.MaxConns = 16
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 5 * time.Minute
	}
	if c.HealthCheck <= 0 {
		c.HealthCheck = 30 * time.Second
	}
	if c.PingAfterIdle == 0 {
		c.PingAfterIdle = 30 * time.Second
	}
	if c.MaxConnLifetime == 0 {
		c.MaxConnLifetime = time.Hour
	}
}

// Pool is a thread-safe pool of *pg2json.Client.
type Pool struct {
	cfg Config

	mu        sync.Mutex
	idle      []*entry
	open      int
	closed    bool
	waiters   []chan *entry

	stopReaper chan struct{}
}

type entry struct {
	c        *pg2json.Client
	lastUsed time.Time
	bornAt   time.Time
}

// New starts a pool. Min idle connections are created eagerly.
func New(cfg Config) (*Pool, error) {
	cfg.applyDefaults()
	p := &Pool{
		cfg:        cfg,
		stopReaper: make(chan struct{}),
	}
	for i := 0; i < cfg.MinIdle; i++ {
		c, err := pg2json.Open(context.Background(), cfg.Config)
		if err != nil {
			p.Close()
			return nil, err
		}
		now := time.Now()
		p.idle = append(p.idle, &entry{c: c, lastUsed: now, bornAt: now})
		p.open++
	}
	go p.reaper()
	return p, nil
}

// ErrClosed is returned by Acquire after Close.
var ErrClosed = errors.New("pool: closed")

// Conn is the handle returned by Acquire. Always call Release or Discard.
type Conn struct {
	*pg2json.Client
	pool *Pool
	e    *entry
	done bool
}

// Release returns the connection to the pool.
func (c *Conn) Release() {
	if c == nil || c.done {
		return
	}
	c.done = true
	c.pool.put(c.e)
}

// Discard closes the connection and decrements the open count. Use this
// when an operation on the connection failed in a way that leaves it in
// an unknown state (network error, mid-query cancellation, etc.).
func (c *Conn) Discard() {
	if c == nil || c.done {
		return
	}
	c.done = true
	_ = c.Client.Close()
	c.pool.discard()
}

// Acquire returns a connection from the pool, blocking up to ctx until one
// is available or the pool is at capacity. Acquire transparently handles:
//   - max-lifetime expiry (the conn is closed and a fresh one is opened)
//   - long-idle ping (catches conns the network silently dropped, e.g.
//     after PgBouncer's server_idle_timeout)
//   - one transparent retry on any failure of the above checks
//
// The retry is bounded — we never loop more than a handful of times so a
// hard backend failure surfaces fast.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	const maxRetries = 3
	for retry := 0; retry < maxRetries; retry++ {
		c, err := p.acquireOnce(ctx)
		if err != nil {
			return nil, err
		}
		// Validate before handing out.
		now := time.Now()
		if p.cfg.MaxConnLifetime > 0 && now.Sub(c.e.bornAt) > p.cfg.MaxConnLifetime {
			c.Discard()
			continue
		}
		if p.cfg.PingAfterIdle > 0 && now.Sub(c.e.lastUsed) > p.cfg.PingAfterIdle {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := c.Client.Ping(pingCtx)
			cancel()
			if err != nil {
				c.Discard()
				continue
			}
		}
		return c, nil
	}
	return nil, errors.New("pool: exhausted retries trying to obtain a healthy connection")
}

func (p *Pool) acquireOnce(ctx context.Context) (*Conn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if n := len(p.idle); n > 0 {
		e := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return &Conn{Client: e.c, pool: p, e: e}, nil
	}
	if p.open < p.cfg.MaxConns {
		p.open++
		p.mu.Unlock()
		c, err := pg2json.Open(ctx, p.cfg.Config)
		if err != nil {
			p.discard()
			return nil, err
		}
		now := time.Now()
		return &Conn{Client: c, pool: p, e: &entry{c: c, lastUsed: now, bornAt: now}}, nil
	}
	ch := make(chan *entry, 1)
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()

	select {
	case e := <-ch:
		if e == nil {
			return nil, ErrClosed
		}
		return &Conn{Client: e.c, pool: p, e: e}, nil
	case <-ctx.Done():
		p.mu.Lock()
		for i, w := range p.waiters {
			if w == ch {
				p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *Pool) put(e *entry) {
	e.lastUsed = time.Now()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = e.c.Close()
		return
	}
	if len(p.waiters) > 0 {
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.mu.Unlock()
		w <- e
		return
	}
	p.idle = append(p.idle, e)
	p.mu.Unlock()
}

func (p *Pool) discard() {
	p.mu.Lock()
	p.open--
	// If anyone is waiting, they should retry: signal with nil so they
	// re-enter Acquire.
	if len(p.waiters) > 0 {
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.mu.Unlock()
		// Open replacement asynchronously so the waiter unblocks fast.
		go func() {
			c, err := pg2json.Open(context.Background(), p.cfg.Config)
			if err != nil {
				w <- nil
				p.mu.Lock()
				if !p.closed {
					p.open-- // cancel the inflight slot
				}
				p.mu.Unlock()
				return
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				_ = c.Close()
				w <- nil
				return
			}
			p.open++
			p.mu.Unlock()
			w <- &entry{c: c, lastUsed: time.Now()}
		}()
		return
	}
	p.mu.Unlock()
}

// Close drains the pool and closes every idle connection. Outstanding
// Acquire callers receive ErrClosed.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	waiters := p.waiters
	p.waiters = nil
	p.mu.Unlock()

	close(p.stopReaper)
	for _, w := range waiters {
		w <- nil
	}
	for _, e := range idle {
		_ = e.c.Close()
	}
	return nil
}

// Stats is a snapshot of pool counters.
type Stats struct {
	Open    int
	Idle    int
	Waiting int
	Max     int
}

func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Open: p.open, Idle: len(p.idle), Waiting: len(p.waiters), Max: p.cfg.MaxConns}
}

func (p *Pool) reaper() {
	ticker := time.NewTicker(p.cfg.HealthCheck)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopReaper:
			return
		case <-ticker.C:
			p.evictIdle()
		}
	}
}

func (p *Pool) evictIdle() {
	cutoff := time.Now().Add(-p.cfg.IdleTimeout)
	p.mu.Lock()
	keep := p.idle[:0]
	var drop []*entry
	for _, e := range p.idle {
		if e.lastUsed.Before(cutoff) && len(keep)+1 > p.cfg.MinIdle {
			drop = append(drop, e)
			p.open--
			continue
		}
		keep = append(keep, e)
	}
	p.idle = keep
	p.mu.Unlock()
	for _, e := range drop {
		_ = e.c.Close()
	}
}

// Compile-time check that the embedded Client satisfies io.Closer if it
// ever stops doing so.
var _ io.Closer = (*pg2json.Client)(nil)
