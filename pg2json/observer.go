package pg2json

import (
	"sync/atomic"
	"time"

	"github.com/arturoeanton/pg2json/internal/pgerr"
)

// Observer is a hook for telemetry. Implementations are called on the hot
// path; keep them allocation-light. Setting an Observer is optional — the
// default no-op observer adds zero overhead.
//
// Implementations MUST be safe for concurrent use even if the Client
// itself is not (multiple Clients may share one Observer through a Pool).
//
// OnNotice is invoked for every server-sent NoticeResponse. Citus surfaces
// important diagnostics from worker nodes this way (shard rebalance
// progress, plan warnings, deprecation notices). The *pgerr.Error argument
// aliases the wire buffer; implementations that keep it past return MUST
// clone the fields they need.
//
// OnQuerySlow is invoked from OnQueryEnd when duration exceeds
// Config.SlowQueryThreshold (if > 0). It is additive to OnQueryEnd; both
// fire for the same query. Useful for a dedicated slow-path alerting
// channel without having to filter every OnQueryEnd event.
type Observer interface {
	OnQueryStart(sql string)
	OnQueryEnd(ev QueryEvent)
	OnNotice(n *pgerr.Error)
	OnQuerySlow(ev QueryEvent)
}

// QueryEvent is the per-query summary delivered to OnQueryEnd. SQLState is
// "" on success, otherwise the SQLSTATE string returned by the server
// (e.g. "26000", "57014"). Retries counts transparent retries performed by
// the driver (SQLSTATE 26000 re-prepare, SQLSTATE 40xxx retryable class).
type QueryEvent struct {
	SQL      string
	Rows     int
	Bytes    int
	Duration time.Duration
	Err      error
	SQLState string
	Retries  int
}

type noopObserver struct{}

func (noopObserver) OnQueryStart(string)     {}
func (noopObserver) OnQueryEnd(QueryEvent)   {}
func (noopObserver) OnNotice(*pgerr.Error)   {}
func (noopObserver) OnQuerySlow(QueryEvent)  {}

// SetObserver installs the telemetry hook. Pass nil to revert to no-op.
func (c *Client) SetObserver(o Observer) {
	if o == nil {
		o = noopObserver{}
	}
	c.observer = o
}

// CounterStats is a tiny lock-free counter snapshot exposed by Stats().
// Useful when you don't want to wire a full Observer just to plot QPS.
type CounterStats struct {
	Queries uint64
	Errors  uint64
	Rows    uint64
	Bytes   uint64
}

// Stats returns a snapshot of the per-Client counters.
func (c *Client) Stats() CounterStats {
	return CounterStats{
		Queries: atomic.LoadUint64(&c.cnt.queries),
		Errors:  atomic.LoadUint64(&c.cnt.errors),
		Rows:    atomic.LoadUint64(&c.cnt.rows),
		Bytes:   atomic.LoadUint64(&c.cnt.bytes),
	}
}

type counters struct {
	queries uint64
	errors  uint64
	rows    uint64
	bytes   uint64
}

func (c *Client) recordEnd(sql string, rows, bytes int, dur time.Duration, err error) {
	atomic.AddUint64(&c.cnt.queries, 1)
	atomic.AddUint64(&c.cnt.rows, uint64(rows))
	atomic.AddUint64(&c.cnt.bytes, uint64(bytes))
	state := ""
	if err != nil {
		atomic.AddUint64(&c.cnt.errors, 1)
		if pe, ok := err.(*pgerr.Error); ok {
			state = pe.Code
		}
	}
	ev := QueryEvent{
		SQL: sql, Rows: rows, Bytes: bytes,
		Duration: dur, Err: err, SQLState: state,
		Retries: c.lastRetries,
	}
	c.observer.OnQueryEnd(ev)
	if thr := c.cfg.SlowQueryThreshold; thr > 0 && dur >= thr {
		c.observer.OnQuerySlow(ev)
	}
	c.lastRetries = 0
}
