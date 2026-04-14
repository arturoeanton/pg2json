package jsonwriter

import (
	"encoding/json"
	"testing"
)

// FuzzAppendString: for arbitrary byte input, AppendString must produce
// output that unmarshals back to the original string — as long as the
// input was valid UTF-8. Invalid-UTF-8 inputs must not panic; the output
// may or may not parse, but it must never crash the writer.
func FuzzAppendString(f *testing.F) {
	f.Add("")
	f.Add("hello")
	f.Add(`quote"inside`)
	f.Add("back\\slash")
	f.Add("\x00\x01\x02control")
	f.Add("tab\there")
	f.Add("newline\nhere")
	f.Add("unicode: café · 日本語 · 🦀")
	f.Add("high-bit bytes: \xff\xfe\xfd")
	f.Add(string([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}))

	f.Fuzz(func(t *testing.T, s string) {
		// Must not panic.
		out := AppendString(nil, s)
		if len(out) < 2 || out[0] != '"' || out[len(out)-1] != '"' {
			t.Fatalf("output not quoted: %q", out)
		}
		// Only assert round-trip equality when the input is valid UTF-8.
		// For invalid bytes the JSON spec doesn't require us to encode
		// them as anything in particular; we just need to not corrupt
		// the structure.
		if ValidUTF8([]byte(s)) {
			var got string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("produced invalid JSON for valid UTF-8 input %q: out=%q err=%v",
					s, out, err)
			}
			if got != s {
				t.Fatalf("round-trip mismatch:\n  in:  %q\n  out: %q\n  back: %q",
					s, out, got)
			}
		} else {
			// Invalid UTF-8: we still must produce SOMETHING that parses
			// as JSON (encoding/json is strict about UTF-8 and will
			// reject invalid sequences). We only check that Unmarshal
			// either accepts it or rejects it cleanly — no panic.
			var got string
			_ = json.Unmarshal(out, &got)
		}
	})
}

// FuzzAppendStringBytes mirrors FuzzAppendString for the []byte path.
func FuzzAppendStringBytes(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte(`"quoted"`))
	f.Add([]byte{0xff, 0xfe, 0x00, '"', '\\'})
	f.Fuzz(func(t *testing.T, b []byte) {
		out := AppendStringBytes(nil, b)
		if len(out) < 2 || out[0] != '"' || out[len(out)-1] != '"' {
			t.Fatalf("output not quoted: %q", out)
		}
		if ValidUTF8(b) {
			var got string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("invalid JSON for valid UTF-8 input: out=%q err=%v", out, err)
			}
			if got != string(b) {
				t.Fatalf("round-trip mismatch: in=%q back=%q", b, got)
			}
		}
	})
}

// FuzzAppendKey: the key prefix must produce a parseable "key": prefix.
func FuzzAppendKey(f *testing.F) {
	f.Add("id")
	f.Add("name with spaces")
	f.Add(`quote"in"key`)
	f.Fuzz(func(t *testing.T, k string) {
		out := AppendKey(nil, k)
		if len(out) < 3 || out[len(out)-1] != ':' {
			t.Fatalf("AppendKey did not terminate with colon: %q", out)
		}
		// Wrap in a minimal object to validate.
		wrapped := append(append([]byte("{"), out...), []byte("1}")...)
		if ValidUTF8([]byte(k)) {
			var m map[string]int
			if err := json.Unmarshal(wrapped, &m); err != nil {
				t.Fatalf("AppendKey produced unparseable prefix for %q: %q err=%v", k, wrapped, err)
			}
			if _, ok := m[k]; !ok {
				t.Fatalf("key not round-tripped: %q → %v", k, m)
			}
		}
	})
}
