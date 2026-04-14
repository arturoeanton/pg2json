package pg2json

import (
	"errors"
	"fmt"
)

// ErrResponseTooLarge is the sentinel returned (wrapped in
// *ResponseTooLargeError) when MaxResponseBytes or MaxResponseRows is hit.
// Use errors.Is to detect it.
var ErrResponseTooLarge = errors.New("pg2json: response exceeded configured cap")

// ResponseTooLargeError is the typed error returned when a query is aborted
// because Config.MaxResponseBytes or Config.MaxResponseRows was crossed.
//
// Committed reports whether any bytes had already been flushed to the user's
// downstream io.Writer when the cap tripped. If true, the partial JSON
// downstream is malformed (truncated mid-array / mid-NDJSON line) and the
// caller is responsible for terminating its own output channel — the same
// contract that applies to mid-stream Citus worker errors.
type ResponseTooLargeError struct {
	Limit     string // "bytes" or "rows"
	LimitVal  int64
	Observed  int64
	Committed bool
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("pg2json: response exceeded %s cap (limit=%d observed=%d committed=%v)",
		e.Limit, e.LimitVal, e.Observed, e.Committed)
}

func (e *ResponseTooLargeError) Unwrap() error { return ErrResponseTooLarge }
