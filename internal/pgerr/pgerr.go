// Package pgerr models PostgreSQL ErrorResponse / NoticeResponse.
package pgerr

import (
	"bytes"
	"fmt"
)

// Error is a structured PostgreSQL error. We only surface the most useful
// fields; the rest are kept in Fields for callers that want them.
type Error struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
	Where    string
	Fields   map[byte]string
}

func (e *Error) Error() string {
	return fmt.Sprintf("postgres: %s %s: %s", e.Severity, e.Code, e.Message)
}

// Parse decodes the body of an ErrorResponse / NoticeResponse message
// (everything after the type byte and length prefix).
func Parse(body []byte) *Error {
	e := &Error{Fields: make(map[byte]string, 8)}
	for len(body) > 0 {
		code := body[0]
		if code == 0 {
			break
		}
		body = body[1:]
		i := bytes.IndexByte(body, 0)
		if i < 0 {
			break
		}
		val := string(body[:i])
		body = body[i+1:]
		e.Fields[code] = val
		switch code {
		case 'S':
			e.Severity = val
		case 'C':
			e.Code = val
		case 'M':
			e.Message = val
		case 'D':
			e.Detail = val
		case 'H':
			e.Hint = val
		case 'W':
			e.Where = val
		}
	}
	return e
}
