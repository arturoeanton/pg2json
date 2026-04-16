package pg2json

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/arturoeanton/pg2json/internal/bufferpool"
	"github.com/arturoeanton/pg2json/internal/pgerr"
	"github.com/arturoeanton/pg2json/internal/protocol"
	"github.com/arturoeanton/pg2json/internal/rows"
)

// checkCaps returns a non-nil *ResponseTooLargeError if either cap was
// crossed. Hot-path: one branch each, no allocs unless tripped.
func (c *Client) checkCaps(out outWriter, rowCount int) *ResponseTooLargeError {
	if max := c.cfg.MaxResponseRows; max > 0 && rowCount >= max {
		return &ResponseTooLargeError{
			Limit: "rows", LimitVal: int64(max),
			Observed: int64(rowCount), Committed: out.Committed(),
		}
	}
	if max := c.cfg.MaxResponseBytes; max > 0 {
		if got := int64(out.BytesOut()); got >= max {
			return &ResponseTooLargeError{
				Limit: "bytes", LimitVal: max,
				Observed: got, Committed: out.Committed(),
			}
		}
	}
	return nil
}

// abortOversize fires a server-side CancelRequest, drains the wire until
// ReadyForQuery so the connection is reusable, and returns the cap error.
// Any ErrorResponse received during the drain (typically SQLSTATE 57014
// query_canceled) is discarded — the cap is the cause we surface.
func (c *Client) abortOversize(capErr *ResponseTooLargeError) error {
	cancelCtx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.SendCancel(cancelCtx)
	ccancel()
	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			// Wire died; the connection is unusable, but the cap error is
			// still the right thing to report. Pool's Discard path will
			// retire this conn.
			return capErr
		}
		switch t {
		case protocol.MsgReadyForQuery:
			return capErr
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		}
	}
}

// Mode controls the JSON shape produced by the streaming engine.
type Mode int

const (
	ModeArray    Mode = iota // [{...},{...}]
	ModeNDJSON               // {...}\n{...}\n
	ModeColumnar             // {"columns":[...],"rows":[[...],[...]]}
)

// QueryJSON runs sql and returns a JSON array of objects in a single
// allocated slice. For large result sets prefer StreamJSON / StreamNDJSON.
func (c *Client) QueryJSON(ctx context.Context, sql string, args ...any) ([]byte, error) {
	bp := bufferpool.Get()
	defer bufferpool.Put(bp)
	*bp = (*bp)[:0]
	w := &bufWriter{p: bp, rowsHint: c.cfg.RowsHint}
	if err := c.runQuery(ctx, w, ModeArray, sql, args); err != nil {
		return nil, err
	}
	out := make([]byte, len(*bp))
	copy(out, *bp)
	return out, nil
}

// StreamJSON streams a JSON array of objects to w.
func (c *Client) StreamJSON(ctx context.Context, w io.Writer, sql string, args ...any) error {
	return c.runQuery(ctx, c.newFlushWriter(w), ModeArray, sql, args)
}

// StreamNDJSON streams newline-delimited JSON to w.
func (c *Client) StreamNDJSON(ctx context.Context, w io.Writer, sql string, args ...any) error {
	return c.runQuery(ctx, c.newFlushWriter(w), ModeNDJSON, sql, args)
}

// StreamColumnar streams the columnar form to w.
func (c *Client) StreamColumnar(ctx context.Context, w io.Writer, sql string, args ...any) error {
	return c.runQuery(ctx, c.newFlushWriter(w), ModeColumnar, sql, args)
}

func (c *Client) newFlushWriter(w io.Writer) *flushingWriter {
	bp := bufferpool.Get()
	return &flushingWriter{
		w:         w,
		threshold: c.cfg.FlushBytes,
		interval:  c.cfg.FlushInterval,
		buf:       *bp,
		src:       bp,
		lastFlush: time.Now(),
		rowsHint:  c.cfg.RowsHint,
	}
}

func (c *Client) runQuery(ctx context.Context, out outWriter, mode Mode, sql string, args []any) (err error) {
	if err = rejectNonSelect(sql); err != nil {
		return err
	}
	// Apply DefaultQueryTimeout only if the caller's ctx has no deadline
	// (so per-request ctx.WithTimeout always wins).
	if d := c.cfg.DefaultQueryTimeout; d > 0 {
		if _, hasDL := ctx.Deadline(); !hasDL {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}
	c.observer.OnQueryStart(sql)
	start := time.Now()
	defer func() {
		c.recordEnd(sql, c.lastRows, out.BytesOut(), time.Since(start), err)
	}()
	if err = c.attachContext(ctx); err != nil {
		return err
	}
	defer c.detachContext()

	// Outer retry loop for SQLSTATE class 40 (serialization_failure,
	// deadlock_detected). Citus surfaces these routinely during shard
	// rebalance and they are safe to retry provided no bytes have been
	// flushed downstream yet. Inner paths keep their own retry logic for
	// SQLSTATE 26000 (PgBouncer backend rotation).
	const maxRetries = 3
	backoff := 10 * time.Millisecond
	for attempt := 0; ; attempt++ {
		if c.cfg.StmtCacheSize < 0 && len(args) == 0 {
			err = c.runSimple(out, mode, sql)
		} else {
			err = c.runCached(out, mode, sql, args)
		}
		if err == nil || !c.cfg.RetryOnSerialization {
			return err
		}
		if attempt+1 >= maxRetries || !isRetryableSerialization(err) || out.Committed() {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return err
		}
		out.Reset()
		c.lastRetries++
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// allBinary reports whether every format code in fmts is 1 (binary).
// When true the Bind message can use the shorthand "one code for all
// columns" form.
func allBinary(fmts []int16) bool {
	if len(fmts) == 0 {
		return false
	}
	for _, f := range fmts {
		if f != 1 {
			return false
		}
	}
	return true
}

// isRetryableSerialization reports whether err is a PostgreSQL class-40
// error the driver may retry: serialization_failure (40001) and
// deadlock_detected (40P01). Other 40xxx codes (e.g. 40002
// transaction_integrity_constraint_violation, 40003
// statement_completion_unknown) are NOT retried — the first is a data
// problem and the second leaves server state ambiguous.
func isRetryableSerialization(err error) bool {
	pe, ok := err.(*pgerr.Error)
	if !ok {
		return false
	}
	return pe.Code == "40001" || pe.Code == "40P01"
}

// --- Simple Query path ---

func (c *Client) runSimple(out outWriter, mode Mode, sql string) error {
	b := c.conn.Begin(protocol.MsgQuery)
	b.CString(sql)
	if err := b.Send(); err != nil {
		return err
	}
	return c.consumeText(out, mode)
}

// --- Cached extended path ---
//
// First call for a given SQL:
//   Parse(name) | Describe Statement(name) | Sync
//   -> ParseComplete, ParameterDescription, RowDescription, ReadyForQuery
//   build plan with binary result formats for known OIDs
//   Bind(name, formats) | Execute | Sync
//   -> BindComplete, DataRow*, CommandComplete, ReadyForQuery
//
// Subsequent calls hit the cache and skip Parse/Describe entirely.
//
// PgBouncer-txn safety: PgBouncer in transaction mode can rotate the
// physical backend between transactions. A cached statement name from a
// previous transaction will not exist on the new backend and Bind will
// fail with SQLSTATE 26000 (invalid_sql_statement_name). We detect that
// specific error, invalidate the cache entry, and retry exactly once with
// a fresh Parse — but only if no output bytes have been flushed to the
// caller yet. If anything was already committed downstream, we surface
// the error so the caller can react (the partial JSON is, by then,
// downstream's problem to discard).
func (c *Client) runCached(out outWriter, mode Mode, sql string, args []any) error {
	for attempt := 0; attempt < 2; attempt++ {
		st := c.stmts.get(sql)
		fromCache := st != nil
		if st == nil {
			var err error
			st, err = c.prepareAndDescribe(sql)
			if err != nil {
				c.stmts.invalidate(sql)
				return err
			}
			c.stmts.put(sql, st)
		}
		err := c.bindExecute(out, mode, st, args)
		if err == nil {
			return nil
		}
		if attempt == 0 && fromCache && isStaleStmtError(err) && !out.Committed() {
			// Backend rotated under us (PgBouncer-txn). Drop the cached
			// name, reset the un-flushed output, and retry from Parse.
			c.stmts.invalidate(sql)
			out.Reset()
			continue
		}
		return err
	}
	return nil
}

// isStaleStmtError reports whether err is a PostgreSQL "prepared statement
// does not exist" error (SQLSTATE 26000). That's the diagnostic PgBouncer
// triggers after a backend rotation in transaction mode.
func isStaleStmtError(err error) bool {
	pe, ok := err.(*pgerr.Error)
	if !ok {
		return false
	}
	return pe.Code == "26000"
}

// prepareAndDescribe runs Parse + Describe Statement + Sync and reads back
// the column metadata.
func (c *Client) prepareAndDescribe(sql string) (*preparedStmt, error) {
	name := c.stmts.nextName()

	// Drain any pending Close messages from previous evictions, then
	// append Parse + Describe + Sync — all in one TCP write.
	for _, dead := range c.stmts.takePendingClose() {
		cl := c.conn.Begin(protocol.MsgClose)
		cl.Byte('S')
		cl.CString(dead)
		cl.Finish()
	}
	p := c.conn.Begin(protocol.MsgParse)
	p.CString(name)
	p.CString(sql)
	p.Int16(0) // 0 parameter type OIDs (server infers)
	p.Finish()
	d := c.conn.Begin(protocol.MsgDescribe)
	d.Byte('S')
	d.CString(name)
	d.Finish()
	c.conn.Begin(protocol.MsgSync).Finish()
	if err := c.conn.Flush(); err != nil {
		return nil, err
	}

	var plan *rows.Plan
	var pgError *pgerr.Error
	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch t {
		case protocol.MsgParseComplete, protocol.MsgParameterDescription,
			protocol.MsgCloseComplete, protocol.MsgNoData:
			// ignore: bookkeeping
		case protocol.MsgRowDescription:
			plan, err = rows.ParseRowDescription(body)
			if err != nil {
				return nil, err
			}
		case protocol.MsgErrorResponse:
			pgError = pgerr.Parse(body)
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgReadyForQuery:
			if pgError != nil {
				return nil, pgError
			}
			if plan == nil {
				return nil, fmt.Errorf("pg2json: prepare returned no row description")
			}
			fmts := rows.PickResultFormats(plan.Columns)
			plan.ApplyFormats(fmts)
			return &preparedStmt{name: name, plan: plan, resultFmts: fmts}, nil
		default:
			return nil, fmt.Errorf("pg2json: unexpected msg %q during describe", t)
		}
	}
}

func (c *Client) bindExecute(out outWriter, mode Mode, st *preparedStmt, args []any) error {
	bd := c.conn.Begin(protocol.MsgBind)
	bd.CString("")        // portal
	bd.CString(st.name)   // statement
	bd.Int16(0)           // all params text
	bd.Int16(int16(len(args)))
	for _, a := range args {
		s, isNull, err := encodeArg(a)
		if err != nil {
			return err
		}
		if isNull {
			bd.Int32(-1)
		} else {
			bd.Int32(int32(len(s)))
			bd.String(s)
		}
	}
	// Per-column result format codes. If every column wants binary, send
	// a single "1" code which the server applies to all columns — saves
	// 2*(N-1) bytes on the Bind.
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
	// Pipeline Execute and Sync into the same TCP write — one syscall
	// instead of three. Nagle is off, so otherwise we'd pay a small
	// per-message overhead and, on high-latency links, per-packet ACK
	// cost.
	ex := c.conn.Begin(protocol.MsgExecute)
	ex.CString("")
	ex.Int32(0)
	ex.Finish()
	c.conn.Begin(protocol.MsgSync).Finish()
	if err := c.conn.Flush(); err != nil {
		return err
	}

	rowCount := 0
	bindOK := false
	headerWritten := false
	var pgError *pgerr.Error
	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		switch t {
		case protocol.MsgBindComplete:
			// Bind succeeded — only now is it safe to start writing
			// output bytes. If Bind fails (e.g. SQLSTATE 26000 on
			// PgBouncer rotation), the buffer is still pristine and the
			// retry path can Reset() it cleanly.
			bindOK = true
		case protocol.MsgDataRow:
			if !headerWritten {
				if err := writeHeader(out, mode, st.plan); err != nil {
					return err
				}
				headerWritten = true
			}
			if err := writeRow(out, mode, st.plan, body, rowCount); err != nil {
				return err
			}
			rowCount++
			if capErr := c.checkCaps(out, rowCount); capErr != nil {
				c.lastRows = rowCount
				return c.abortOversize(capErr)
			}
		case protocol.MsgCommandComplete, protocol.MsgEmptyQueryResponse,
			protocol.MsgPortalSuspended, protocol.MsgCloseComplete:
		case protocol.MsgRowDescription:
			newPlan, err := rows.ParseRowDescriptionForFormats(body, st.resultFmts)
			if err != nil {
				return err
			}
			st.plan = newPlan
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgErrorResponse:
			pgError = pgerr.Parse(body)
		case protocol.MsgReadyForQuery:
			c.lastRows = rowCount
			if pgError != nil {
				return pgError
			}
			_ = bindOK // BindComplete is implied by reaching here without error
			if !headerWritten {
				if err := writeHeader(out, mode, st.plan); err != nil {
					return err
				}
			}
			if err := writeFooter(out, mode); err != nil {
				return err
			}
			return out.Flush()
		default:
			return fmt.Errorf("pg2json: unexpected msg %q during execute", t)
		}
	}
}

// consumeText is used by the simple-query path. RowDescription always
// arrives in text format here.
func (c *Client) consumeText(out outWriter, mode Mode) error {
	var plan *rows.Plan
	rowCount := 0
	headerWritten := false
	var pgError *pgerr.Error
	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		switch t {
		case protocol.MsgRowDescription:
			plan, err = rows.ParseRowDescription(body)
			if err != nil {
				return err
			}
		case protocol.MsgDataRow:
			if plan == nil {
				return fmt.Errorf("pg2json: DataRow before RowDescription")
			}
			if !headerWritten {
				if err := writeHeader(out, mode, plan); err != nil {
					return err
				}
				headerWritten = true
			}
			if err := writeRow(out, mode, plan, body, rowCount); err != nil {
				return err
			}
			rowCount++
			if capErr := c.checkCaps(out, rowCount); capErr != nil {
				c.lastRows = rowCount
				return c.abortOversize(capErr)
			}
		case protocol.MsgCommandComplete, protocol.MsgEmptyQueryResponse:
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgErrorResponse:
			pgError = pgerr.Parse(body)
		case protocol.MsgReadyForQuery:
			c.lastRows = rowCount
			if pgError != nil {
				return pgError
			}
			if !headerWritten {
				p := plan
				if p == nil {
					p = &rows.Plan{ColumnsArrayHeader: []byte(`{"columns":[],"rows":[`)}
				}
				if err := writeHeader(out, mode, p); err != nil {
					return err
				}
			}
			if err := writeFooter(out, mode); err != nil {
				return err
			}
			return out.Flush()
		default:
			return fmt.Errorf("pg2json: unexpected msg %q during simple query", t)
		}
	}
}

func writeHeader(out outWriter, mode Mode, plan *rows.Plan) error {
	switch mode {
	case ModeArray:
		return out.WriteByte('[')
	case ModeNDJSON:
		return nil
	case ModeColumnar:
		_, err := out.Write(plan.ColumnsArrayHeader)
		return err
	}
	return nil
}

func writeRow(out outWriter, mode Mode, plan *rows.Plan, body []byte, idx int) error {
	scratch := out.Buf()
	startLen := len(scratch)
	switch mode {
	case ModeArray:
		if idx > 0 {
			scratch = append(scratch, ',')
		}
		var err error
		scratch, err = rows.AppendObject(scratch, body, plan)
		if err != nil {
			return err
		}
	case ModeNDJSON:
		var err error
		scratch, err = rows.AppendObject(scratch, body, plan)
		if err != nil {
			return err
		}
		scratch = append(scratch, '\n')
	case ModeColumnar:
		if idx > 0 {
			scratch = append(scratch, ',')
		}
		var err error
		scratch, err = rows.AppendArray(scratch, body, plan)
		if err != nil {
			return err
		}
	}
	out.SetBuf(scratch)
	if idx == 0 {
		out.GrowForRow(len(scratch) - startLen)
	}
	return out.MaybeFlush()
}

func writeFooter(out outWriter, mode Mode) error {
	switch mode {
	case ModeArray:
		return out.WriteByte(']')
	case ModeColumnar:
		_, err := out.Write([]byte("]}"))
		return err
	}
	return nil
}

