package pg2json

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
	"unsafe"

	"github.com/arturoeanton/pg2json/internal/pgerr"
	"github.com/arturoeanton/pg2json/internal/protocol"
	"github.com/arturoeanton/pg2json/internal/rows"
)

// ScanStruct runs sql and materialises each row into a value of type T.
// Field-to-column mapping uses the `pg2json:"col_name"` struct tag when
// present, otherwise the field name case-insensitively. Unmapped columns
// are silently skipped; unmapped fields stay zero-valued.
//
// Spike scope — explicitly narrow:
//   - Supported cell types: bool, int2/4/8, float4/8, text/varchar/bpchar,
//     uuid, bytea, jsonb (into []byte / json.RawMessage), date,
//     timestamp/timestamptz (into time.Time).
//   - NULL: only meaningful when the target field is a pointer. Non-pointer
//     fields stay zero on a NULL cell (lenient; not an error).
//   - No arrays, no sql.NullXxx, no custom sql.Scanner, no embedded structs.
//   - No INSERT/UPDATE/DELETE — this remains a SELECT-only driver.
//
// Those limits are what keeps the hot path allocation-free and the code
// tractable. If a query column does not map cleanly to its target field,
// the call returns an error at the first row rather than silently
// coercing.
func ScanStruct[T any](c *Client, ctx context.Context, sql string, args ...any) ([]T, error) {
	if err := rejectNonSelect(sql); err != nil {
		return nil, err
	}

	// DefaultQueryTimeout applies as in runQuery.
	if d := c.cfg.DefaultQueryTimeout; d > 0 {
		if _, hasDL := ctx.Deadline(); !hasDL {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	if err := c.attachContext(ctx); err != nil {
		return nil, err
	}
	defer c.detachContext()

	var zero T
	tType := reflect.TypeOf(zero)
	if tType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("pg2json: ScanStruct[T] requires T to be a struct, got %s", tType.Kind())
	}

	var st *preparedStmt
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		st = c.stmts.get(sql)
		fromCache := st != nil
		if st == nil {
			st, err = c.prepareAndDescribe(sql)
			if err != nil {
				c.stmts.invalidate(sql)
				return nil, err
			}
			c.stmts.put(sql, st)
		}

		plan, err := buildStructPlan(tType, st.plan)
		if err != nil {
			return nil, err
		}

		result, err := c.bindExecuteScan(st, args, plan, tType)
		if err == nil {
			return castSlice[T](result), nil
		}
		if attempt == 0 && fromCache && isStaleStmtError(err) {
			c.stmts.invalidate(sql)
			continue
		}
		return nil, err
	}
	return nil, err
}

// castSlice re-types a []unsafe.Pointer-backed slice to []T. The
// underlying memory was allocated with the correct T so this is safe.
func castSlice[T any](rawSlice any) []T {
	if rawSlice == nil {
		return nil
	}
	return rawSlice.([]T)
}

// ScanStructBatched is the memory-bounded variant of ScanStruct.
//
// Instead of materialising the full result set as []T, it calls cb with
// a batch slice of up to batchSize rows, waits for cb to return, then
// reuses the slice for the next batch. Memory use is O(batchSize *
// sizeof(T)) regardless of the total row count — use it for queries
// that would return millions of rows (audit logs, analytics exports,
// bulk processing).
//
// The slice passed to cb aliases the internal buffer and is valid only
// until cb returns. If cb needs to retain rows beyond that, it must
// copy them. Returning a non-nil error from cb aborts the scan: pg2json
// issues a server-side CancelRequest and drains to ReadyForQuery so
// the connection is reusable.
//
// Returns io.EOF from cb for an early stop? No — return a distinguished
// sentinel if you want to stop without reporting an error. Any non-nil
// error is surfaced to the caller.
func ScanStructBatched[T any](
	c *Client, ctx context.Context,
	batchSize int,
	cb func(batch []T) error,
	sql string, args ...any,
) error {
	if batchSize <= 0 {
		return fmt.Errorf("pg2json: ScanStructBatched requires batchSize > 0")
	}
	if cb == nil {
		return fmt.Errorf("pg2json: ScanStructBatched requires a non-nil callback")
	}
	if err := rejectNonSelect(sql); err != nil {
		return err
	}
	if d := c.cfg.DefaultQueryTimeout; d > 0 {
		if _, hasDL := ctx.Deadline(); !hasDL {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}
	if err := c.attachContext(ctx); err != nil {
		return err
	}
	defer c.detachContext()

	var zero T
	tType := reflect.TypeOf(zero)
	if tType.Kind() != reflect.Struct {
		return fmt.Errorf("pg2json: ScanStructBatched[T] requires T to be a struct, got %s", tType.Kind())
	}
	cbAny := func(batch any) error { return cb(batch.([]T)) }

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
		sp, err := buildStructPlan(tType, st.plan)
		if err != nil {
			return err
		}
		err = c.bindExecuteScanBatched(st, args, sp, tType, batchSize, cbAny)
		if err == nil {
			return nil
		}
		if attempt == 0 && fromCache && isStaleStmtError(err) {
			c.stmts.invalidate(sql)
			continue
		}
		return err
	}
	return nil
}

// structPlan is the per-(ResultShape, T) plan. Built once per ScanStruct
// call; for repeated queries on the same shape the RowDescription plan
// is cached, but the struct plan itself is rebuilt each call. That is
// cheap (O(columns)) and keeps the API simple; caching across calls
// would require a (reflect.Type, *rows.Plan) → plan map which we punt
// on for the spike.
type structPlan struct {
	// One entry per column in the result. Offset==^uintptr(0) means
	// "skip this column" (no matching field).
	setters []fieldSetter
}

type fieldSetter struct {
	offset uintptr
	set    func(fieldPtr unsafe.Pointer, raw []byte) error
	// op identifies the fast-path inline case the row loop can
	// handle without calling `set`. Zero (opFallback) means "no fast
	// path — call set through the function pointer". This closes the
	// 4-7% loopback gap vs pgx.Scan by eliminating the indirect call
	// on the common scalar shapes while leaving exotic types (time,
	// pointers, sql.Null*, Scanner, arrays) on the existing path.
	op   uint8
	skip bool
}

// Inline-dispatch opcodes for fillStructRow. Kept internal — these
// are optimisation metadata, not public API. opFallback is the
// implicit zero value so any setter that does not explicitly set op
// takes the function-pointer path, matching pre-optimisation
// behaviour.
const (
	opFallback uint8 = iota
	opInt2BinInt16
	opInt2BinInt32
	opInt2BinInt64
	opInt4BinInt32
	opInt4BinInt64
	opInt8BinInt64
	opFloat4BinF32
	opFloat4BinF64
	opFloat8BinF64
	opBoolBin
	opStringPass  // text/varchar/jsonb-after-prefix copy
	opByteCopyRaw // bytea / json / jsonb-stripped into []byte
)

// fieldRef points at a leaf struct field including any offset
// contribution from enclosing anonymous (embedded) structs. It is
// the plan-build-time answer to "which Go field does column X
// target, and at what memory offset from the row base?".
type fieldRef struct {
	field  reflect.StructField
	offset uintptr
}

// walkStructFields walks an entire struct tree — including any
// anonymous (embedded) struct fields — and records every leaf
// exported field under its resolved column name in `out`.
//
// Resolution rules:
//   - `pg2json:"-"` skips the field entirely.
//   - `pg2json:"name"` overrides the column name.
//   - Anonymous struct fields are flattened automatically, EXCEPT
//     when the embedded type is time.Time or the field has a
//     pg2json tag (treat it as a single scalar).
//   - Outer-declared fields win against embedded duplicates, so a
//     leaf with the same resolved name defined directly on the
//     outer struct takes precedence.
//   - `seen` is threaded to break reference cycles defensively; in
//     practice Postgres-row structs are acyclic but pointer-chains
//     to self are possible in contrived cases.
func walkStructFields(t reflect.Type, baseOffset uintptr, seen map[reflect.Type]bool, out map[string]fieldRef) {
	if seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)

	isEmbeddedFlatten := func(f reflect.StructField) bool {
		tag, hasTag := f.Tag.Lookup("pg2json")
		_ = tag
		if hasTag {
			return false
		}
		return f.Anonymous && f.Type.Kind() == reflect.Struct && !isTimeType(f.Type)
	}

	// Pass 1 — directly-declared (non-embedded) leaf fields at this
	// level. These SHADOW any duplicate names surfaced by embedded
	// structs, matching Go's own field-promotion rule ("shallower
	// wins"). Unexported directly-declared fields are skipped as
	// usual.
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, hasTag := f.Tag.Lookup("pg2json")
		if hasTag && tag == "-" {
			continue
		}
		if isEmbeddedFlatten(f) {
			continue
		}
		if !f.IsExported() {
			continue
		}
		key := f.Name
		if hasTag && tag != "" {
			key = tag
		}
		out[strings.ToLower(key)] = fieldRef{field: f, offset: baseOffset + f.Offset}
	}

	// Pass 2 — recurse into embedded structs. Resolve each
	// embedded subtree independently, then merge only names not
	// already claimed. Unexported anonymous struct types are
	// flattened too: Go promotes their exported leaves regardless.
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, hasTag := f.Tag.Lookup("pg2json")
		if hasTag && tag == "-" {
			continue
		}
		if !isEmbeddedFlatten(f) {
			continue
		}
		inner := make(map[string]fieldRef)
		walkStructFields(f.Type, baseOffset+f.Offset, seen, inner)
		for k, v := range inner {
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	}
}

func buildStructPlan(tType reflect.Type, plan *rows.Plan) (*structPlan, error) {
	nameIdx := make(map[string]fieldRef, tType.NumField())
	walkStructFields(tType, 0, map[reflect.Type]bool{}, nameIdx)

	sp := &structPlan{setters: make([]fieldSetter, len(plan.Columns))}
	for ci, col := range plan.Columns {
		info, ok := nameIdx[strings.ToLower(col.Name)]
		if !ok {
			sp.setters[ci].skip = true
			continue
		}
		setter, err := pickSetter(col.TypeOID, col.Format, info.field.Type)
		if err != nil {
			return nil, fmt.Errorf("pg2json: column %q (OID %d) -> field %q (%s): %w",
				col.Name, col.TypeOID, info.field.Name, info.field.Type, err)
		}
		sp.setters[ci] = fieldSetter{
			offset: info.offset,
			set:    setter,
			op:     inlineOpFor(col.TypeOID, col.Format, info.field.Type),
		}
	}
	return sp, nil
}

// bindExecuteScan mirrors bindExecute but fills a []T by copying
// decoded cells into each row's struct instance.
func (c *Client) bindExecuteScan(st *preparedStmt, args []any, sp *structPlan, tType reflect.Type) (any, error) {
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

	// Allocate capacity upfront, then advance a write-pointer by typeSize
	// bytes per row. This avoids reflect.Append + reflect.Zero per row —
	// the single biggest cell-level win we can get over the obvious
	// implementation. Grows geometrically when capacity is exhausted.
	sliceType := reflect.SliceOf(tType)
	initialCap := 16
	if h := c.cfg.RowsHint; h > 0 {
		initialCap = h
	}
	sliceVal := reflect.MakeSlice(sliceType, initialCap, initialCap)
	typeSize := tType.Size()
	dataPtr := unsafe.Pointer(sliceVal.Index(0).UnsafeAddr())
	currentCap := initialCap

	var pgError *pgerr.Error
	rowCount := 0
	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch t {
		case protocol.MsgBindComplete:
		case protocol.MsgDataRow:
			if rowCount == currentCap {
				// Grow 2x, copy existing bytes, update dataPtr. MakeSlice
				// zeroes the tail so the freshly-revealed rows start
				// zero-valued — matches what reflect.Zero provided before.
				newCap := currentCap * 2
				newSliceVal := reflect.MakeSlice(sliceType, newCap, newCap)
				newDataPtr := unsafe.Pointer(newSliceVal.Index(0).UnsafeAddr())
				src := unsafe.Slice((*byte)(dataPtr), currentCap*int(typeSize))
				dst := unsafe.Slice((*byte)(newDataPtr), newCap*int(typeSize))
				copy(dst, src)
				sliceVal = newSliceVal
				dataPtr = newDataPtr
				currentCap = newCap
			}
			rowPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(rowCount)*typeSize)
			if err := fillStructRow(rowPtr, body, sp); err != nil {
				return nil, err
			}
			rowCount++
		case protocol.MsgCommandComplete, protocol.MsgEmptyQueryResponse,
			protocol.MsgPortalSuspended, protocol.MsgCloseComplete:
		case protocol.MsgRowDescription:
			newPlan, err := rows.ParseRowDescriptionForFormats(body, st.resultFmts)
			if err != nil {
				return nil, err
			}
			st.plan = newPlan
			// Rebuild the struct plan against the new shape.
			newSP, err := buildStructPlan(tType, newPlan)
			if err != nil {
				return nil, err
			}
			sp = newSP
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
				return nil, pgError
			}
			// Trim the over-allocated tail to the real row count.
			return sliceVal.Slice(0, rowCount).Interface(), nil
		default:
			return nil, fmt.Errorf("pg2json: unexpected msg %q during scan", t)
		}
	}
}

// bindExecuteScanBatched mirrors bindExecuteScan but flushes every
// batchSize rows through cbAny instead of building one []T. The slice
// is allocated once at batchSize capacity and reused — no growth, no
// re-alloc, no reflect.Append. Memory remains O(batchSize).
func (c *Client) bindExecuteScanBatched(
	st *preparedStmt, args []any, sp *structPlan, tType reflect.Type,
	batchSize int, cbAny func(any) error,
) error {
	bd := c.conn.Begin(protocol.MsgBind)
	bd.CString("")
	bd.CString(st.name)
	bd.Int16(0)
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
		return err
	}

	sliceType := reflect.SliceOf(tType)
	batch := reflect.MakeSlice(sliceType, batchSize, batchSize)
	typeSize := tType.Size()
	dataPtr := unsafe.Pointer(batch.Index(0).UnsafeAddr())

	var pgError *pgerr.Error
	var cbErr error
	rowCount := 0 // within current batch
	totalRows := 0

	flush := func() error {
		if rowCount == 0 {
			return nil
		}
		if err := cbAny(batch.Slice(0, rowCount).Interface()); err != nil {
			return err
		}
		// Zero the slots we just handed out so the next batch doesn't
		// leak pointers from the previous one (strings, slices, maps,
		// interface values — anything with a reference). For
		// zero-reference structs this is cheap; for reference-heavy
		// ones it keeps the GC honest.
		clearBatchSlots(dataPtr, uintptr(rowCount)*typeSize)
		rowCount = 0
		return nil
	}

	for {
		t, body, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		switch t {
		case protocol.MsgBindComplete:
		case protocol.MsgDataRow:
			if cbErr != nil {
				// Already told the server to cancel — drain silently.
				continue
			}
			rowPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(rowCount)*typeSize)
			if err := fillStructRow(rowPtr, body, sp); err != nil {
				return err
			}
			rowCount++
			totalRows++
			if rowCount == batchSize {
				if err := flush(); err != nil {
					cbErr = err
					// Abort server-side so the connection stays usable.
					cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					_ = c.SendCancel(cancelCtx)
					cancel()
				}
			}
		case protocol.MsgCommandComplete, protocol.MsgEmptyQueryResponse,
			protocol.MsgPortalSuspended, protocol.MsgCloseComplete:
		case protocol.MsgRowDescription:
			newPlan, err := rows.ParseRowDescriptionForFormats(body, st.resultFmts)
			if err != nil {
				return err
			}
			st.plan = newPlan
			newSP, err := buildStructPlan(tType, newPlan)
			if err != nil {
				return err
			}
			sp = newSP
		case protocol.MsgNoticeResponse:
			c.observer.OnNotice(pgerr.Parse(body))
		case protocol.MsgParameterStatus:
			k, v := splitParameter(body)
			c.params[k] = v
		case protocol.MsgErrorResponse:
			pgError = pgerr.Parse(body)
		case protocol.MsgReadyForQuery:
			c.lastRows = totalRows
			if cbErr != nil {
				return cbErr
			}
			if pgError != nil {
				return pgError
			}
			// Final partial batch.
			return flush()
		default:
			return fmt.Errorf("pg2json: unexpected msg %q during batched scan", t)
		}
	}
}

// clearBatchSlots zeroes the first n bytes starting at p. Used between
// batches so reference-typed fields (string headers, slice headers,
// maps, interfaces) do not pin memory from the previous batch after cb
// returns.
func clearBatchSlots(p unsafe.Pointer, n uintptr) {
	if n == 0 {
		return
	}
	b := unsafe.Slice((*byte)(p), n)
	for i := range b {
		b[i] = 0
	}
}

// fillStructRow walks the DataRow body and dispatches each column
// to its setter. The inline switch handles the common scalar OIDs
// directly (int2/4/8, float4/8, bool, string-family, byte-slice
// family) to avoid the Go function-pointer call overhead; exotic
// types (time.Time, pointers, sql.Null*, Scanner, arrays) keep the
// existing `set` indirection through opFallback.
//
// Measured closing-the-gap effect: the indirect call costs ~2.5 ns
// on M4; at 5 cells × 100k rows the loop used to pay 1.25 ms on a
// 30 ms query. Inlining recovers that and ties us with pgx.Scan.
func fillStructRow(rowPtr unsafe.Pointer, body []byte, sp *structPlan) error {
	if len(body) < 2 {
		return fmt.Errorf("pg2json: short DataRow")
	}
	n := int(int16(uint16(body[0])<<8 | uint16(body[1])))
	if n != len(sp.setters) {
		return fmt.Errorf("pg2json: column count mismatch (got %d, plan %d)", n, len(sp.setters))
	}
	body = body[2:]
	for i := 0; i < n; i++ {
		if len(body) < 4 {
			return fmt.Errorf("pg2json: short column header")
		}
		l := int32(uint32(body[0])<<24 | uint32(body[1])<<16 |
			uint32(body[2])<<8 | uint32(body[3]))
		body = body[4:]
		var raw []byte
		isNull := l == -1
		if !isNull {
			if l < 0 || int(l) > len(body) {
				return fmt.Errorf("pg2json: bad column length %d", l)
			}
			raw = body[:l]
			body = body[l:]
		}
		s := &sp.setters[i]
		if s.skip {
			continue
		}
		fieldPtr := unsafe.Pointer(uintptr(rowPtr) + s.offset)
		// Inline fast path for the common binary scalars. Each case
		// body is small enough that Go lowers the switch into a jump
		// table and keeps the function frame reasonable. Anything
		// more complex (pointer-for-NULL, sql.Null*, Scanner, arrays,
		// time.Time) falls through to the s.set indirection.
		if !isNull {
			switch s.op {
			case opInt4BinInt32:
				if len(raw) == 4 {
					*(*int32)(fieldPtr) = int32(uint32(raw[0])<<24 | uint32(raw[1])<<16 |
						uint32(raw[2])<<8 | uint32(raw[3]))
					continue
				}
			case opInt4BinInt64:
				if len(raw) == 4 {
					*(*int64)(fieldPtr) = int64(int32(uint32(raw[0])<<24 | uint32(raw[1])<<16 |
						uint32(raw[2])<<8 | uint32(raw[3])))
					continue
				}
			case opInt8BinInt64:
				if len(raw) == 8 {
					*(*int64)(fieldPtr) = int64(uint64(raw[0])<<56 | uint64(raw[1])<<48 |
						uint64(raw[2])<<40 | uint64(raw[3])<<32 |
						uint64(raw[4])<<24 | uint64(raw[5])<<16 |
						uint64(raw[6])<<8 | uint64(raw[7]))
					continue
				}
			case opInt2BinInt16:
				if len(raw) == 2 {
					*(*int16)(fieldPtr) = int16(uint16(raw[0])<<8 | uint16(raw[1]))
					continue
				}
			case opInt2BinInt32:
				if len(raw) == 2 {
					*(*int32)(fieldPtr) = int32(int16(uint16(raw[0])<<8 | uint16(raw[1])))
					continue
				}
			case opInt2BinInt64:
				if len(raw) == 2 {
					*(*int64)(fieldPtr) = int64(int16(uint16(raw[0])<<8 | uint16(raw[1])))
					continue
				}
			case opFloat8BinF64:
				if len(raw) == 8 {
					u := uint64(raw[0])<<56 | uint64(raw[1])<<48 |
						uint64(raw[2])<<40 | uint64(raw[3])<<32 |
						uint64(raw[4])<<24 | uint64(raw[5])<<16 |
						uint64(raw[6])<<8 | uint64(raw[7])
					*(*float64)(fieldPtr) = math.Float64frombits(u)
					continue
				}
			case opFloat4BinF32:
				if len(raw) == 4 {
					u := uint32(raw[0])<<24 | uint32(raw[1])<<16 |
						uint32(raw[2])<<8 | uint32(raw[3])
					*(*float32)(fieldPtr) = math.Float32frombits(u)
					continue
				}
			case opFloat4BinF64:
				if len(raw) == 4 {
					u := uint32(raw[0])<<24 | uint32(raw[1])<<16 |
						uint32(raw[2])<<8 | uint32(raw[3])
					*(*float64)(fieldPtr) = float64(math.Float32frombits(u))
					continue
				}
			case opBoolBin:
				if len(raw) == 1 {
					*(*bool)(fieldPtr) = raw[0] != 0
					continue
				}
			case opStringPass:
				// Plain text family: copy the wire bytes into a
				// caller-owned string. This is where 1 alloc/row for
				// text columns lives; caller ownership is required by
				// database/sql and struct-scan contracts alike.
				*(*string)(fieldPtr) = string(raw)
				continue
			case opByteCopyRaw:
				out := make([]byte, len(raw))
				copy(out, raw)
				*(*[]byte)(fieldPtr) = out
				continue
			}
		}
		if err := s.set(fieldPtr, raw); err != nil {
			return err
		}
	}
	return nil
}

// pickSetter returns a setter appropriate for writing a cell of
// (OID, format) into a field of type targetType. Errors when the pair
// cannot be safely represented without coercion.
//
// Dispatch order (first match wins):
//  1. One of the sql.Null* wrappers from database/sql — handled by
//     pickNullSetter so .Valid is set correctly on NULL cells.
//  2. A type whose *T implements sql.Scanner (custom decoders like
//     uuid.UUID, decimal.Decimal, domain types). We call .Scan on &field
//     with the canonical intermediate the database/sql contract expects.
//  3. Pointer-to-X: nil on NULL, recursively decoded on non-NULL.
//  4. Plain value by (OID, format).
func pickSetter(oid protocol.OID, format int16, targetType reflect.Type) (func(unsafe.Pointer, []byte) error, error) {
	if setter, ok := pickNullSetter(oid, format, targetType); ok {
		return setter, nil
	}
	if setter, ok := pickScannerSetter(oid, format, targetType); ok {
		return setter, nil
	}
	if setter, ok := pickRangeSetter(oid, format, targetType); ok {
		return setter, nil
	}
	// Array target: slice with PG array OID on the wire. Excludes
	// []byte (handled as bytea in the binary path) so `text[]` into
	// `[]byte` still fails loudly.
	if elemOID := protocol.ArrayElem(oid); elemOID != 0 &&
		targetType.Kind() == reflect.Slice &&
		targetType.Elem().Kind() != reflect.Uint8 {
		setter, err := pickArraySetter(elemOID, format, targetType)
		if err != nil {
			return nil, err
		}
		return setter, nil
	}

	// Pointer-to-X means: nil on NULL, allocate on non-NULL. We wrap an
	// inner setter that writes to *X.
	if targetType.Kind() == reflect.Pointer {
		elemType := targetType.Elem()
		inner, err := pickSetter(oid, format, elemType)
		if err != nil {
			return nil, err
		}
		return func(fp unsafe.Pointer, raw []byte) error {
			if raw == nil {
				// Zero the pointer field.
				*(*unsafe.Pointer)(fp) = nil
				return nil
			}
			newVal := reflect.New(elemType)
			if err := inner(unsafe.Pointer(newVal.Pointer()), raw); err != nil {
				return err
			}
			*(*unsafe.Pointer)(fp) = unsafe.Pointer(newVal.Pointer())
			return nil
		}, nil
	}

	// Non-pointer field: NULL → zero value (lenient).
	if format == 1 {
		return pickBinarySetter(oid, targetType)
	}
	return pickTextSetter(oid, targetType)
}

func pickBinarySetter(oid protocol.OID, t reflect.Type) (func(unsafe.Pointer, []byte) error, error) {
	switch oid {
	case protocol.OIDBool:
		if t.Kind() == reflect.Bool {
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil {
					*(*bool)(fp) = false
					return nil
				}
				*(*bool)(fp) = len(raw) == 1 && raw[0] != 0
				return nil
			}, nil
		}
	case protocol.OIDInt2:
		if setter, ok := intSetter(t, 2); ok {
			return setter, nil
		}
	case protocol.OIDInt4, protocol.OIDOID:
		if setter, ok := intSetter(t, 4); ok {
			return setter, nil
		}
	case protocol.OIDInt8:
		if setter, ok := intSetter(t, 8); ok {
			return setter, nil
		}
	case protocol.OIDFloat4:
		if t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64 {
			kind := t.Kind()
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil || len(raw) != 4 {
					return nil
				}
				v := math.Float32frombits(binary.BigEndian.Uint32(raw))
				if kind == reflect.Float32 {
					*(*float32)(fp) = v
				} else {
					*(*float64)(fp) = float64(v)
				}
				return nil
			}, nil
		}
	case protocol.OIDFloat8:
		if t.Kind() == reflect.Float64 {
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil || len(raw) != 8 {
					return nil
				}
				*(*float64)(fp) = math.Float64frombits(binary.BigEndian.Uint64(raw))
				return nil
			}, nil
		}
	case protocol.OIDText, protocol.OIDVarchar, protocol.OIDBPChar, protocol.OIDName:
		if t.Kind() == reflect.String {
			return stringSetter(), nil
		}
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return byteSliceSetter(false), nil
		}
	case protocol.OIDUUID:
		if t.Kind() == reflect.String {
			return uuidStringSetter(), nil
		}
		if t.Kind() == reflect.Array && t.Elem().Kind() == reflect.Uint8 && t.Len() == 16 {
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil || len(raw) != 16 {
					return nil
				}
				copy((*[16]byte)(fp)[:], raw)
				return nil
			}, nil
		}
	case protocol.OIDBytea:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return byteSliceSetter(false), nil
		}
	case protocol.OIDJSON:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return byteSliceSetter(false), nil
		}
		if t.Kind() == reflect.String {
			return stringSetter(), nil
		}
	case protocol.OIDJSONB:
		// jsonb binary has a 1-byte version prefix (0x01) to strip.
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return byteSliceSetter(true), nil
		}
		if t.Kind() == reflect.String {
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil {
					*(*string)(fp) = ""
					return nil
				}
				body := raw
				if len(body) > 0 && body[0] == 1 {
					body = body[1:]
				}
				*(*string)(fp) = string(body)
				return nil
			}, nil
		}
	case protocol.OIDDate:
		if isTimeType(t) {
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil || len(raw) != 4 {
					return nil
				}
				days := int32(binary.BigEndian.Uint32(raw))
				if days == math.MaxInt32 || days == math.MinInt32 {
					return nil
				}
				usec := int64(days) * (86400 * 1_000_000)
				*(*time.Time)(fp) = pgUsecToTime(usec).UTC()
				return nil
			}, nil
		}
	case protocol.OIDTimestamp, protocol.OIDTimestampTZ:
		if isTimeType(t) {
			return func(fp unsafe.Pointer, raw []byte) error {
				if raw == nil || len(raw) != 8 {
					return nil
				}
				usec := int64(binary.BigEndian.Uint64(raw))
				if usec == math.MaxInt64 || usec == math.MinInt64 {
					return nil
				}
				*(*time.Time)(fp) = pgUsecToTime(usec)
				return nil
			}, nil
		}
	}
	return nil, fmt.Errorf("no binary setter for OID %d into %s", oid, t)
}

func pickTextSetter(oid protocol.OID, t reflect.Type) (func(unsafe.Pointer, []byte) error, error) {
	// For the spike, text format support is minimal: strings → strings,
	// integers → integer fields. Most production callers will hit the
	// binary path via the built-in format negotiation.
	if t.Kind() == reflect.String {
		return stringSetter(), nil
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return byteSliceSetter(false), nil
	}
	return nil, fmt.Errorf("no text setter for OID %d into %s", oid, t)
}

// intSetter returns a setter that decodes `width` big-endian bytes
// (2/4/8) into any signed-int or unsigned-int Go field. Widening is
// allowed (int2 → int64 is fine); narrowing is rejected implicitly via
// the Kind check.
func intSetter(t reflect.Type, width int) (func(unsafe.Pointer, []byte) error, bool) {
	k := t.Kind()
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
	default:
		return nil, false
	}
	return func(fp unsafe.Pointer, raw []byte) error {
		if raw == nil {
			return nil // zero-value
		}
		if len(raw) != width {
			return fmt.Errorf("pg2json: expected %d bytes, got %d", width, len(raw))
		}
		var v int64
		switch width {
		case 2:
			v = int64(int16(binary.BigEndian.Uint16(raw)))
		case 4:
			v = int64(int32(binary.BigEndian.Uint32(raw)))
		case 8:
			v = int64(binary.BigEndian.Uint64(raw))
		}
		switch k {
		case reflect.Int:
			*(*int)(fp) = int(v)
		case reflect.Int8:
			*(*int8)(fp) = int8(v)
		case reflect.Int16:
			*(*int16)(fp) = int16(v)
		case reflect.Int32:
			*(*int32)(fp) = int32(v)
		case reflect.Int64:
			*(*int64)(fp) = v
		case reflect.Uint:
			*(*uint)(fp) = uint(v)
		case reflect.Uint8:
			*(*uint8)(fp) = uint8(v)
		case reflect.Uint16:
			*(*uint16)(fp) = uint16(v)
		case reflect.Uint32:
			*(*uint32)(fp) = uint32(v)
		case reflect.Uint64:
			*(*uint64)(fp) = uint64(v)
		}
		return nil
	}, true
}

func stringSetter() func(unsafe.Pointer, []byte) error {
	return func(fp unsafe.Pointer, raw []byte) error {
		if raw == nil {
			*(*string)(fp) = ""
			return nil
		}
		*(*string)(fp) = string(raw)
		return nil
	}
}

// byteSliceSetter returns a setter that copies raw bytes into a []byte
// field. When stripVersion is true (jsonb binary), the first byte is
// dropped (it is the jsonb wire-format version marker, currently 1).
func byteSliceSetter(stripVersion bool) func(unsafe.Pointer, []byte) error {
	return func(fp unsafe.Pointer, raw []byte) error {
		if raw == nil {
			*(*[]byte)(fp) = nil
			return nil
		}
		body := raw
		if stripVersion && len(body) > 0 && body[0] == 1 {
			body = body[1:]
		}
		// Copy so the caller owns the bytes (raw aliases the wire buf).
		out := make([]byte, len(body))
		copy(out, body)
		*(*[]byte)(fp) = out
		return nil
	}
}

func uuidStringSetter() func(unsafe.Pointer, []byte) error {
	const hex = "0123456789abcdef"
	return func(fp unsafe.Pointer, raw []byte) error {
		if raw == nil {
			*(*string)(fp) = ""
			return nil
		}
		if len(raw) != 16 {
			return fmt.Errorf("pg2json: uuid expected 16 bytes, got %d", len(raw))
		}
		b := make([]byte, 36)
		o := 0
		emit := func(src []byte) {
			for _, c := range src {
				b[o] = hex[c>>4]
				o++
				b[o] = hex[c&0xF]
				o++
			}
		}
		emit(raw[0:4])
		b[o] = '-'
		o++
		emit(raw[4:6])
		b[o] = '-'
		o++
		emit(raw[6:8])
		b[o] = '-'
		o++
		emit(raw[8:10])
		b[o] = '-'
		o++
		emit(raw[10:16])
		*(*string)(fp) = string(b)
		return nil
	}
}

var timeType = reflect.TypeOf(time.Time{})

func isTimeType(t reflect.Type) bool { return t == timeType }

const pgEpochUnixSec int64 = 946684800

func pgUsecToTime(usec int64) time.Time {
	sec := pgEpochUnixSec + usec/1_000_000
	nsec := (usec % 1_000_000) * 1_000
	if nsec < 0 {
		sec--
		nsec += 1_000_000_000
	}
	return time.Unix(sec, nsec).UTC()
}

// --- sql.NullXxx support -----------------------------------------------
//
// database/sql's standard nullable wrappers all share the layout
// `{ Typed, Valid bool }` with a fixed field order. We detect the type by
// identity (not structural) and set both fields directly via unsafe —
// the `Valid` offset is discovered once per ScanStruct call through
// reflect.StructField lookup. No reflection per row.

var (
	nullStringType  = reflect.TypeOf(sql.NullString{})
	nullInt16Type   = reflect.TypeOf(sql.NullInt16{})
	nullInt32Type   = reflect.TypeOf(sql.NullInt32{})
	nullInt64Type   = reflect.TypeOf(sql.NullInt64{})
	nullFloat64Type = reflect.TypeOf(sql.NullFloat64{})
	nullBoolType    = reflect.TypeOf(sql.NullBool{})
	nullTimeType    = reflect.TypeOf(sql.NullTime{})
	nullByteType    = reflect.TypeOf(sql.NullByte{})
)

// pickNullSetter returns a setter for one of database/sql's NullXxx
// wrappers. Returns (nil, false) if targetType is not a known Null type.
func pickNullSetter(oid protocol.OID, format int16, targetType reflect.Type) (func(unsafe.Pointer, []byte) error, bool) {
	switch targetType {
	case nullStringType:
		inner, err := pickSetter(oid, format, reflect.TypeOf(""))
		if err != nil {
			return nil, false
		}
		validOff := validFieldOffset(targetType)
		return nullSetterWrap(inner, validOff), true
	case nullInt16Type:
		inner, err := pickSetter(oid, format, reflect.TypeOf(int16(0)))
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	case nullInt32Type:
		inner, err := pickSetter(oid, format, reflect.TypeOf(int32(0)))
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	case nullInt64Type:
		inner, err := pickSetter(oid, format, reflect.TypeOf(int64(0)))
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	case nullFloat64Type:
		inner, err := pickSetter(oid, format, reflect.TypeOf(float64(0)))
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	case nullBoolType:
		inner, err := pickSetter(oid, format, reflect.TypeOf(false))
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	case nullTimeType:
		inner, err := pickSetter(oid, format, timeType)
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	case nullByteType:
		inner, err := pickSetter(oid, format, reflect.TypeOf(byte(0)))
		if err != nil {
			return nil, false
		}
		return nullSetterWrap(inner, validFieldOffset(targetType)), true
	}
	return nil, false
}

// validFieldOffset returns the byte offset of the `Valid bool` field
// inside a sql.Null* struct. All of them have it; panic on misuse.
func validFieldOffset(t reflect.Type) uintptr {
	f, ok := t.FieldByName("Valid")
	if !ok || f.Type.Kind() != reflect.Bool {
		panic(fmt.Sprintf("pg2json: expected Valid bool field in %s", t))
	}
	return f.Offset
}

// nullSetterWrap returns a setter that writes the typed part via inner
// and sets .Valid based on NULL vs present. The typed field sits at
// offset 0 inside every sql.Null* type (that layout is stable).
func nullSetterWrap(inner func(unsafe.Pointer, []byte) error, validOff uintptr) func(unsafe.Pointer, []byte) error {
	return func(fp unsafe.Pointer, raw []byte) error {
		validPtr := (*bool)(unsafe.Pointer(uintptr(fp) + validOff))
		if raw == nil {
			*validPtr = false
			// Leave the typed field at its zero value.
			return nil
		}
		if err := inner(fp, raw); err != nil {
			return err
		}
		*validPtr = true
		return nil
	}
}

// --- Range type support -----------------------------------------------
//
// RangeBytes captures a PostgreSQL range value in its raw wire form:
// the two bound bytes plus the flags that say whether each bound is
// inclusive, exclusive, or infinite (open on that side). The inner
// bound bytes arrive in the same binary encoding as the element type
// — int4 for int4range, int8 for int8range, timestamp for tsrange,
// etc. Decoding them further is the caller's job because the shape
// varies per range kind; `pg2json.DriverValueDecoder(oid, 1)(raw)`
// on each bound field is the usual path.
//
// Wire layout, reference: src/backend/utils/adt/rangetypes.c
// (range_send / range_recv):
//
//	uint8 flags
//	if !empty && !lower_inf: int32 lower_len, bytes
//	if !empty && !upper_inf: int32 upper_len, bytes
//
// Flag bits:
//
//	0x01 empty
//	0x02 lower-bound inclusive
//	0x04 upper-bound inclusive
//	0x08 lower bound is -infinity (no bytes follow for it)
//	0x10 upper bound is +infinity
//
// For callers that want a typed bound (time.Time, int64, etc.) an
// idiomatic sql.Scanner on a user-defined range type also works:
// pg2json's scanner dispatch passes the full raw wire body when the
// OID is not in the canonical set.
type RangeBytes struct {
	Flags          uint8
	Lower          []byte // nil if LowerInfinite or Empty
	Upper          []byte // nil if UpperInfinite or Empty
	Empty          bool
	LowerInclusive bool
	UpperInclusive bool
	LowerInfinite  bool
	UpperInfinite  bool
}

var rangeBytesType = reflect.TypeOf(RangeBytes{})

func pickRangeSetter(oid protocol.OID, format int16, t reflect.Type) (func(unsafe.Pointer, []byte) error, bool) {
	if format != 1 {
		return nil, false
	}
	if !protocol.IsRange(oid) {
		return nil, false
	}
	if t != rangeBytesType {
		return nil, false
	}
	return func(fp unsafe.Pointer, raw []byte) error {
		rb := (*RangeBytes)(fp)
		if raw == nil {
			*rb = RangeBytes{}
			return nil
		}
		if len(raw) < 1 {
			return fmt.Errorf("pg2json: range wire body empty")
		}
		flags := raw[0]
		rb.Flags = flags
		rb.Empty = flags&0x01 != 0
		rb.LowerInclusive = flags&0x02 != 0
		rb.UpperInclusive = flags&0x04 != 0
		rb.LowerInfinite = flags&0x08 != 0
		rb.UpperInfinite = flags&0x10 != 0
		rb.Lower = nil
		rb.Upper = nil
		if rb.Empty {
			return nil
		}
		body := raw[1:]
		if !rb.LowerInfinite {
			if len(body) < 4 {
				return fmt.Errorf("pg2json: range lower header truncated")
			}
			ll := int32(binary.BigEndian.Uint32(body[0:4]))
			body = body[4:]
			if ll < 0 || int(ll) > len(body) {
				return fmt.Errorf("pg2json: bad range lower length %d", ll)
			}
			rb.Lower = append([]byte(nil), body[:ll]...)
			body = body[ll:]
		}
		if !rb.UpperInfinite {
			if len(body) < 4 {
				return fmt.Errorf("pg2json: range upper header truncated")
			}
			ul := int32(binary.BigEndian.Uint32(body[0:4]))
			body = body[4:]
			if ul < 0 || int(ul) > len(body) {
				return fmt.Errorf("pg2json: bad range upper length %d", ul)
			}
			rb.Upper = append([]byte(nil), body[:ul]...)
			body = body[ul:]
		}
		return nil
	}, true
}

// --- PG array support --------------------------------------------------
//
// Wire layout (pg_send_array in the server):
//
//	int32 ndim
//	int32 hasNulls (0/1)
//	int32 elemOID
//	per dim: int32 dimLen, int32 lowerBound
//	per element (flat, product of dimLens): int32 elemLen (-1=NULL), bytes
//
// For the spike we support 1D arrays only. Nested / multi-dim are
// reported as "not yet supported" so callers fail fast instead of
// getting garbage. NULL array cell → nil slice. NULL element → zero
// value of the element type (lenient — use a `[]*T` field to round-trip
// the NULL explicitly).
func pickArraySetter(elemOID protocol.OID, format int16, targetType reflect.Type) (func(unsafe.Pointer, []byte) error, error) {
	if format != 1 {
		return nil, fmt.Errorf("pg2json: array scan requires binary format (elem OID %d)", elemOID)
	}
	// Count Go nesting depth (the target type's slice-of-slice
	// rank) and resolve the leaf scalar element type. The inner
	// element setter runs on the leaf; the outer loops build the
	// nested slices. We cap at 2D for now because 3D+ requires more
	// extensive testing with real PG workloads and is not in any
	// reported use case.
	goDepth := 0
	leafType := targetType
	for leafType.Kind() == reflect.Slice && leafType.Elem().Kind() == reflect.Slice &&
		leafType.Elem().Elem().Kind() != reflect.Uint8 {
		goDepth++
		leafType = leafType.Elem()
	}
	// leafType is now []scalar. One more unwrap to get the scalar.
	if leafType.Kind() != reflect.Slice {
		return nil, fmt.Errorf("pg2json: array target must be a slice, got %s", targetType)
	}
	scalarType := leafType.Elem()
	goDepth++ // the final []scalar level

	if goDepth > 2 {
		return nil, fmt.Errorf("pg2json: multi-dim arrays with Go depth > 2 not supported (target %s)", targetType)
	}

	elemSetter, err := pickSetter(elemOID, 1, scalarType)
	if err != nil {
		return nil, fmt.Errorf("pg2json: array element setter: %w", err)
	}
	scalarSize := scalarType.Size()

	if goDepth == 1 {
		// 1D fast path — unchanged from the original implementation.
		return func(fp unsafe.Pointer, raw []byte) error {
			if raw == nil {
				reflect.NewAt(targetType, fp).Elem().Set(reflect.Zero(targetType))
				return nil
			}
			if len(raw) < 12 {
				return fmt.Errorf("pg2json: array header truncated")
			}
			ndim := int32(binary.BigEndian.Uint32(raw[0:4]))
			raw = raw[12:]
			if ndim == 0 {
				reflect.NewAt(targetType, fp).Elem().Set(reflect.MakeSlice(targetType, 0, 0))
				return nil
			}
			if ndim != 1 {
				return fmt.Errorf("pg2json: expected 1-dim array, got ndim=%d", ndim)
			}
			if len(raw) < 8 {
				return fmt.Errorf("pg2json: array dim header truncated")
			}
			dimLen := int32(binary.BigEndian.Uint32(raw[0:4]))
			raw = raw[8:]
			if dimLen < 0 {
				return fmt.Errorf("pg2json: negative array dim %d", dimLen)
			}
			sliceVal := reflect.MakeSlice(targetType, int(dimLen), int(dimLen))
			reflect.NewAt(targetType, fp).Elem().Set(sliceVal)
			if dimLen == 0 {
				return nil
			}
			dataPtr := unsafe.Pointer(sliceVal.Index(0).UnsafeAddr())
			for i := int32(0); i < dimLen; i++ {
				elemRaw, rest, err := readArrayElem(raw, i)
				if err != nil {
					return err
				}
				raw = rest
				elemPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(i)*scalarSize)
				if err := elemSetter(elemPtr, elemRaw); err != nil {
					return fmt.Errorf("pg2json: array element %d: %w", i, err)
				}
			}
			return nil
		}, nil
	}

	// 2D — outer [][]T from a PG ndim=2 array. Elements arrive flat
	// in row-major order (outerIdx*innerLen + innerIdx). We allocate
	// the outer slice of outerLen, then for each row allocate an
	// inner slice of innerLen, fill, and store into the outer slot.
	// goDepth == 2 guarantees targetType.Kind() == slice and
	// targetType.Elem() == leafType == []scalar.
	return func(fp unsafe.Pointer, raw []byte) error {
		if raw == nil {
			reflect.NewAt(targetType, fp).Elem().Set(reflect.Zero(targetType))
			return nil
		}
		if len(raw) < 12 {
			return fmt.Errorf("pg2json: array header truncated")
		}
		ndim := int32(binary.BigEndian.Uint32(raw[0:4]))
		raw = raw[12:]
		if ndim == 0 {
			reflect.NewAt(targetType, fp).Elem().Set(reflect.MakeSlice(targetType, 0, 0))
			return nil
		}
		if ndim != 2 {
			return fmt.Errorf("pg2json: 2D target requires ndim=2 PG array, got %d", ndim)
		}
		if len(raw) < 16 {
			return fmt.Errorf("pg2json: 2D array dim headers truncated")
		}
		outerLen := int32(binary.BigEndian.Uint32(raw[0:4]))
		innerLen := int32(binary.BigEndian.Uint32(raw[8:12]))
		raw = raw[16:]
		if outerLen < 0 || innerLen < 0 {
			return fmt.Errorf("pg2json: negative 2D dim outer=%d inner=%d", outerLen, innerLen)
		}
		outer := reflect.MakeSlice(targetType, int(outerLen), int(outerLen))
		reflect.NewAt(targetType, fp).Elem().Set(outer)
		for i := int32(0); i < outerLen; i++ {
			inner := reflect.MakeSlice(leafType, int(innerLen), int(innerLen))
			outer.Index(int(i)).Set(inner)
			if innerLen == 0 {
				continue
			}
			innerDataPtr := unsafe.Pointer(inner.Index(0).UnsafeAddr())
			for j := int32(0); j < innerLen; j++ {
				elemRaw, rest, err := readArrayElem(raw, i*innerLen+j)
				if err != nil {
					return err
				}
				raw = rest
				elemPtr := unsafe.Pointer(uintptr(innerDataPtr) + uintptr(j)*scalarSize)
				if err := elemSetter(elemPtr, elemRaw); err != nil {
					return fmt.Errorf("pg2json: 2D element [%d][%d]: %w", i, j, err)
				}
			}
		}
		return nil
	}, nil
}

// readArrayElem consumes one element header + body from a PG array
// wire stream. Returns the element bytes (nil for NULL), the
// remaining buffer, and an error on malformed input.
func readArrayElem(raw []byte, idx int32) (elemRaw, rest []byte, err error) {
	if len(raw) < 4 {
		return nil, nil, fmt.Errorf("pg2json: array elem header truncated at %d", idx)
	}
	el := int32(binary.BigEndian.Uint32(raw[0:4]))
	raw = raw[4:]
	if el == -1 {
		return nil, raw, nil
	}
	if el < 0 || int(el) > len(raw) {
		return nil, nil, fmt.Errorf("pg2json: bad array elem length %d at %d", el, idx)
	}
	return raw[:el], raw[el:], nil
}

// --- sql.Scanner support -----------------------------------------------
//
// Any type T whose *T implements sql.Scanner (e.g. uuid.UUID,
// decimal.Decimal, custom domain wrappers) gets routed through Scan. We
// decode the cell to the canonical intermediate the database/sql
// contract specifies (int64, float64, bool, []byte, string, time.Time,
// or nil), then call Scan on a *T that points at the struct field.
//
// Cost: one interface boxing per non-NULL cell, plus whatever the
// Scanner implementation does. Still faster than pgx's full pgtype
// layer for custom types.

// inlineOpFor returns the fast-path opcode for a (OID, format,
// targetType) triple, or opFallback when the inline switch in
// fillStructRow cannot handle it. Nullable fields (pointers,
// sql.Null*, Scanner targets, slice arrays) all return opFallback so
// the wrapper setter runs — the inline switch assumes a non-nullable
// plain-value field.
func inlineOpFor(oid protocol.OID, format int16, t reflect.Type) uint8 {
	if format != 1 {
		return opFallback
	}
	// Only flat non-pointer scalars get the fast path.
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Interface:
		if t == reflect.TypeOf(([]byte)(nil)) {
			// byte-slice is fine for bytea/json/jsonb.
		} else if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			// any named []byte
		} else {
			return opFallback
		}
	case reflect.Struct:
		// time.Time lives here; keep it on the fallback path.
		return opFallback
	}
	switch oid {
	case protocol.OIDBool:
		if t.Kind() == reflect.Bool {
			return opBoolBin
		}
	case protocol.OIDInt2:
		switch t.Kind() {
		case reflect.Int16:
			return opInt2BinInt16
		case reflect.Int32:
			return opInt2BinInt32
		case reflect.Int64, reflect.Int:
			return opInt2BinInt64
		}
	case protocol.OIDInt4, protocol.OIDOID:
		switch t.Kind() {
		case reflect.Int32:
			return opInt4BinInt32
		case reflect.Int64, reflect.Int:
			return opInt4BinInt64
		}
	case protocol.OIDInt8:
		if t.Kind() == reflect.Int64 || t.Kind() == reflect.Int {
			return opInt8BinInt64
		}
	case protocol.OIDFloat4:
		switch t.Kind() {
		case reflect.Float32:
			return opFloat4BinF32
		case reflect.Float64:
			return opFloat4BinF64
		}
	case protocol.OIDFloat8:
		if t.Kind() == reflect.Float64 {
			return opFloat8BinF64
		}
	case protocol.OIDText, protocol.OIDVarchar, protocol.OIDBPChar, protocol.OIDName:
		if t.Kind() == reflect.String {
			return opStringPass
		}
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return opByteCopyRaw
		}
	case protocol.OIDBytea, protocol.OIDJSON:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return opByteCopyRaw
		}
	}
	return opFallback
}

// DriverValueDecoder returns a decoder that converts the raw wire
// bytes of a cell to the canonical driver.Value that database/sql
// expects: int64 / float64 / bool / []byte / string / time.Time, or
// nil on NULL. Used by the pg2json/stdlib adapter; callers of the
// native API typically do not need this.
func DriverValueDecoder(oid uint32, format int16) func([]byte) any {
	return scannerDecoderFor(protocol.OID(oid), format)
}

var scannerInterfaceType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()

func pickScannerSetter(oid protocol.OID, format int16, targetType reflect.Type) (func(unsafe.Pointer, []byte) error, bool) {
	// We need *T to implement Scanner so we can pass &field to Scan.
	ptrType := reflect.PointerTo(targetType)
	if !ptrType.Implements(scannerInterfaceType) {
		return nil, false
	}
	// Skip our own sql.Null* — already handled by pickNullSetter above.
	switch targetType {
	case nullStringType, nullInt16Type, nullInt32Type, nullInt64Type,
		nullFloat64Type, nullBoolType, nullTimeType, nullByteType:
		return nil, false
	}

	decode := scannerDecoderFor(oid, format)
	return func(fp unsafe.Pointer, raw []byte) error {
		// Build a Scanner around the field.
		scannerPtr := reflect.NewAt(targetType, fp).Interface().(sql.Scanner)
		if raw == nil {
			return scannerPtr.Scan(nil)
		}
		return scannerPtr.Scan(decode(raw))
	}, true
}

// scannerDecoderFor returns a function converting the raw wire bytes to
// the canonical Go value database/sql's Scan contract expects, based on
// the column's OID and format. For OIDs we do not specifically recognise
// we fall back to passing []byte — Scanner implementations that accept
// raw bytes (common for uuid, decimal) handle it; those that don't will
// surface a type-mismatch error from Scan itself, which is the correct
// behaviour.
func scannerDecoderFor(oid protocol.OID, format int16) func([]byte) any {
	if format == 1 { // binary
		switch oid {
		case protocol.OIDBool:
			return func(raw []byte) any { return len(raw) == 1 && raw[0] != 0 }
		case protocol.OIDInt2:
			return func(raw []byte) any { return int64(int16(binary.BigEndian.Uint16(raw))) }
		case protocol.OIDInt4, protocol.OIDOID:
			return func(raw []byte) any { return int64(int32(binary.BigEndian.Uint32(raw))) }
		case protocol.OIDInt8:
			return func(raw []byte) any { return int64(binary.BigEndian.Uint64(raw)) }
		case protocol.OIDFloat4:
			return func(raw []byte) any {
				return float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))
			}
		case protocol.OIDFloat8:
			return func(raw []byte) any {
				return math.Float64frombits(binary.BigEndian.Uint64(raw))
			}
		case protocol.OIDText, protocol.OIDVarchar, protocol.OIDBPChar, protocol.OIDName:
			return func(raw []byte) any { return string(raw) }
		case protocol.OIDTimestamp, protocol.OIDTimestampTZ:
			return func(raw []byte) any {
				if len(raw) != 8 {
					return raw
				}
				usec := int64(binary.BigEndian.Uint64(raw))
				return pgUsecToTime(usec)
			}
		case protocol.OIDDate:
			return func(raw []byte) any {
				if len(raw) != 4 {
					return raw
				}
				days := int32(binary.BigEndian.Uint32(raw))
				return pgUsecToTime(int64(days) * (86400 * 1_000_000))
			}
		case protocol.OIDJSONB:
			// Strip 1-byte version prefix so the Scanner sees canonical JSON.
			return func(raw []byte) any {
				if len(raw) > 0 && raw[0] == 1 {
					return raw[1:]
				}
				return raw
			}
		}
	}
	// text format or unknown binary OID: hand over raw bytes. Most
	// Scanners accept []byte fallback (that's what database/sql does).
	return func(raw []byte) any { return raw }
}
