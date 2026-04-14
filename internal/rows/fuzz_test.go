package rows

import "testing"

// FuzzParseRowDescription: arbitrary byte sequences fed to the
// RowDescription parser must never panic. Malformed input must return
// an error (non-nil), valid input returns a non-nil *Plan.
func FuzzParseRowDescription(f *testing.F) {
	// Seed: well-formed single-column RowDescription body.
	//   int16 field_count = 1
	//   cstring name = "a\0"
	//   int32  table_oid = 0
	//   int16  attr_num  = 0
	//   int32  type_oid  = 23  (int4)
	//   int16  type_len  = 4
	//   int32  type_mod  = -1
	//   int16  format    = 0
	f.Add([]byte{
		0, 1, // n = 1
		'a', 0,
		0, 0, 0, 0,
		0, 0,
		0, 0, 0, 23,
		0, 4,
		0xff, 0xff, 0xff, 0xff,
		0, 0,
	})
	f.Add([]byte{})
	f.Add([]byte{0, 0}) // n = 0
	f.Add([]byte{0xff, 0xff}) // n = -1 (int16)

	f.Fuzz(func(t *testing.T, body []byte) {
		plan, err := ParseRowDescription(body)
		if err == nil {
			if plan == nil {
				t.Fatal("nil plan with nil error")
			}
		}
	})
}
