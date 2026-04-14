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
type Observer interface {
	OnQueryStart(sql string)
	OnQueryEnd(ev QueryEvent)
}

// QueryEvent is the per-query summary delivered to OnQueryEnd. SQLState is
// "" on success, otherwise the SQLSTATE string returned by the server
// (e.g. "26000", "57014").
type QueryEvent struct {
	SQL      string
	Rows     int
	Bytes    int
	Duration time.Duration
	Err      error
	SQLState string
}

type noopObserver struct{}

func (noopObserver) OnQueryStart(string)   {}
func (noopObserver) OnQueryEnd(QueryEvent) {}

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
	c.observer.OnQueryEnd(QueryEvent{
		SQL: sql, Rows: rows, Bytes: bytes,
		Duration: dur, Err: err, SQLState: state,
	})
}
