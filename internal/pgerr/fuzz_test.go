package pgerr

import "testing"

// FuzzParse: arbitrary byte sequences fed into Parse must never panic
// and must always produce a non-nil *Error. The parser intentionally
// tolerates truncation — invalid input yields a partially-populated
// error value, not a crash.
func FuzzParse(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{'S', 'E', 'R', 'R', 'O', 'R', 0, 'C', '4', '2', '0', '0', '0', 0, 0})
	f.Add([]byte{'M', 'h', 'i', 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		e := Parse(b)
		if e == nil {
			t.Fatal("Parse returned nil")
		}
		// Error() must not panic regardless of content.
		_ = e.Error()
	})
}
