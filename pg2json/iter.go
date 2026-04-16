package pg2json

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/arturoeanton/pg2json/internal/pgerr"
	"github.com/arturoeanton/pg2json/internal/protocol"
	"github.com/arturoeanton/pg2json/internal/rows"
)

// Iterator streams DataRows one at a time from an in-flight SELECT.
//
// Lifecycle: obtain via (*Client).RawQuery, then call NextRaw() until
// it returns io.EOF or an error. Call Close() to drain any unread rows
// and return the underlying connection to a usable state — the Client
// cannot run another query until Close is called, so use defer.
//
// This is the foundation for the database/sql adapter (driver.Rows
// expects row-at-a-time semantics) and for callers that want to weave
// pg2json decoding into their own iteration loop without buffering the
// full result.
type Iterator struct {
	c        *Client
	st       *preparedStmt
	plan     *rows.Plan
	ctxStop  context.CancelFunc // release DefaultQueryTimeout wrapper, if any

	bound    bool          // BindComplete seen
	done     bool          // ReadyForQuery received
	err      error         // sticky error
	pgErr    *pgerr.Error
	rowCount int
}

// RawQuery runs sql and returns an Iterator for lazy row consumption.
// The Iterator pins the Client's connection until Close() is called;
// do not issue another query on the same Client in the interim.
//
// The returned DataRow bodies alias the internal wire buffer and are
// valid only until the next NextRaw() call, matching the contract of
// internal/wire.Conn.ReadMessage.
func (c *Client) RawQuery(ctx context.Context, sql string, args ...any) (*Iterator, error) {
	if err := rejectNonSelect(sql); err != nil {
		return nil, err
	}
	// Apply DefaultQueryTimeout. The cancel is stashed on the Iterator
	// and invoked from Close(), not from a defer in this function —
	// the query is still in flight when we return.
	var ctxStop context.CancelFunc
	if d := c.cfg.DefaultQueryTimeout; d > 0 {
		if _, hasDL := ctx.Deadline(); !hasDL {
			ctx, ctxStop = context.WithTimeout(ctx, d)
		}
	}
	if err := c.attachContext(ctx); err != nil {
		if ctxStop != nil {
			ctxStop()
		}
		return nil, err
	}

	// Prepare + retry on PgBouncer-txn stale cache. Matches the pattern
	// in runCached; inlined here because the iterator owns the
	// connection past the prepare step and cannot reuse the existing
	// bindExecute machinery.
	var st *preparedStmt
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		st = c.stmts.get(sql)
		fromCache := st != nil
		if st == nil {
			st, err = c.prepareAndDescribe(sql)
			if err != nil {
				c.detachContext()
				if ctxStop != nil {
					ctxStop()
				}
				c.stmts.invalidate(sql)
				return nil, err
			}
			c.stmts.put(sql, st)
		}
		it, err := c.startRawExecute(st, args)
		if err == nil {
			it.ctxStop = ctxStop
			return it, nil
		}
		if attempt == 0 && fromCache && isStaleStmtError(err) {
			c.stmts.invalidate(sql)
			continue
		}
		c.detachContext()
		if ctxStop != nil {
			ctxStop()
		}
		return nil, err
	}
	c.detachContext()
	if ctxStop != nil {
		ctxStop()
	}
	return nil, err
}

// startRawExecute sends Bind+Execute+Sync and reads messages until
// BindComplete. After that, NextRaw pulls the DataRows lazily.
func (c *Client) startRawExecute(st *preparedStmt, args []any) (*Iterator, error) {
	bd := c.conn.Begin(protocol.MsgBind)
	bd.CString("")
	bd.CString(st.name)
	bd.Int16(0)
	bd.Int16(int16(len(args)))
	for _, a := range args {
		s, isNull, err := encodeArg(a)
		if err != nil {
			return nil, err
		}
		if isNull {
			bd.Int32(-1)
		} else {
			bd.Int32(int32(len(s)))
			bd.String(s)
		}
	}
	if allBinary(st.resultFmts) {
		bd.Int16(1)
		bd.Int16(1)
	} else {
		bd.Int16(int16(len(st.resultFmts)))
		for _, f := range st.resultFmts {
			bd.Int16(f)
		}
	}
	bd.Finish()
	ex := c.conn.Begin(protocol.MsgExecute)
	ex.CString("")
	ex.Int32(0)
	ex.Finish()
	c.conn.Begin(protocol.MsgSync).Finish()
	if err := c.conn.Flush(); err != nil {
		return nil, err
	}

	it := &Iterator{c: c, st: st, plan: st.plan}
	// Pump until BindComplete or error. Anything after that is a
	// DataRow stream NextRaw reads on demand.
	for !it.bound {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch t {
		case protocol.MsgBindComplete:
			it.bound = true
		case protocol.MsgRowDescription:
			newPlan, err := rows.ParseRowDescriptionForFormats(body, st.resultFmts)
			if err != nil {
				return nil, err
			}
			st.plan = newPlan
			it.plan = newPlan
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgErrorResponse:
			// Bind failed (e.g. SQLSTATE 26000 stale stmt). Drain to
			// ReadyForQuery so caller can retry on a fresh stmt.
			it.pgErr = pgerr.Parse(body)
			if err := it.drainUntilReady(); err != nil {
				return nil, err
			}
			return nil, it.pgErr
		default:
			return nil, fmt.Errorf("pg2json: unexpected msg %q during bind phase", t)
		}
	}
	return it, nil
}

// Plan returns the compiled row plan for the active result set. Valid
// once at least one NextRaw call has returned (or for queries that
// ship RowDescription pre-Bind, immediately).
func (it *Iterator) Plan() *rows.Plan { return it.plan }

// NextRaw returns the body of the next DataRow, or io.EOF when the
// result set is exhausted. The returned slice aliases the wire buffer
// and is invalidated by the next NextRaw / Close call.
func (it *Iterator) NextRaw() ([]byte, error) {
	if it.done {
		if it.err != nil {
			return nil, it.err
		}
		return nil, io.EOF
	}
	if it.err != nil {
		return nil, it.err
	}
	for {
		t, body, err := it.c.conn.ReadMessage()
		if err != nil {
			it.err = err
			return nil, err
		}
		switch t {
		case protocol.MsgDataRow:
			it.rowCount++
			return body, nil
		case protocol.MsgCommandComplete, protocol.MsgEmptyQueryResponse,
			protocol.MsgPortalSuspended, protocol.MsgCloseComplete:
			// keep reading
		case protocol.MsgRowDescription:
			newPlan, err := rows.ParseRowDescriptionForFormats(body, it.st.resultFmts)
			if err != nil {
				it.err = err
				return nil, err
			}
			it.st.plan = newPlan
			it.plan = newPlan
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			it.c.params[k] = v
		case protocol.MsgNoticeResponse:
			it.c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgErrorResponse:
			it.pgErr = pgerr.Parse(body)
		case protocol.MsgReadyForQuery:
			it.done = true
			it.c.lastRows = it.rowCount
			it.releaseContext()
			if it.pgErr != nil {
				it.err = it.pgErr
				return nil, it.pgErr
			}
			return nil, io.EOF
		default:
			it.err = fmt.Errorf("pg2json: unexpected msg %q during iterate", t)
			return nil, it.err
		}
	}
}

// Close drains any unread rows and returns the underlying connection
// to a usable state. Safe to call multiple times and after an error.
// Callers should always defer Close.
//
// When Close is called mid-stream (the caller stopped reading before
// io.EOF) we issue a CancelRequest to stop the backend from producing
// more rows. The server then emits ErrorResponse 57014
// (canceling_statement_due_to_user_request) followed by ReadyForQuery.
// That specific SQLSTATE is a direct consequence of our own cancel and
// is not surfaced to the caller — Close returns nil in that case.
func (it *Iterator) Close() error {
	if it.done {
		return nil
	}
	userCanceled := false
	if it.c.pid != 0 {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = it.c.SendCancel(cctx)
		cancel()
		userCanceled = true
	}
	err := it.drainUntilReady()
	if err != nil && userCanceled {
		if pe, ok := err.(*pgerr.Error); ok && pe.Code == "57014" {
			return nil
		}
	}
	return err
}

func (it *Iterator) drainUntilReady() error {
	for {
		t, body, err := it.c.conn.ReadMessage()
		if err != nil {
			it.err = err
			it.done = true
			it.releaseContext()
			return err
		}
		switch t {
		case protocol.MsgReadyForQuery:
			it.done = true
			it.releaseContext()
			if it.pgErr != nil {
				return it.pgErr
			}
			return nil
		case protocol.MsgErrorResponse:
			it.pgErr = pgerr.Parse(body)
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			it.c.params[k] = v
		case protocol.MsgNoticeResponse:
			it.c.observer.OnNotice(pgerr.Parse(body))
		}
	}
}

func (it *Iterator) releaseContext() {
	it.c.detachContext()
	if it.ctxStop != nil {
		it.ctxStop()
		it.ctxStop = nil
	}
}

