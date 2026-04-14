package pg2json

import (
	"errors"
	"testing"

	"github.com/arturoeanton/pg2json/internal/pgerr"
)

func TestIsRetryableSerialization(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"40001", true},  // serialization_failure
		{"40P01", true},  // deadlock_detected
		{"40002", false}, // integrity_constraint_violation
		{"40003", false}, // statement_completion_unknown
		{"26000", false}, // stale prepared stmt (handled elsewhere)
		{"", false},      // not a PG error
	}
	for _, c := range cases {
		var err error = &pgerr.Error{Code: c.code}
		if c.code == "" {
			err = errors.New("network boom")
		}
		if got := isRetryableSerialization(err); got != c.want {
			t.Fatalf("code=%q want=%v got=%v", c.code, c.want, got)
		}
	}
	// Nil must not panic.
	if isRetryableSerialization(nil) {
		t.Fatal("nil error must not be retryable")
	}
}
