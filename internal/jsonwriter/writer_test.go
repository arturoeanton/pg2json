package jsonwriter

import (
	"encoding/json"
	"testing"
)

func TestAppendString_RoundTrip(t *testing.T) {
	cases := []string{
		"", "hello", "with \"quote\"", "back\\slash",
		"control\x01\x02\x1f", "emoji 🚀 ok", "tab\there\nnewline",
	}
	for _, c := range cases {
		got := AppendString(nil, c)
		var out string
		if err := json.Unmarshal(got, &out); err != nil {
			t.Fatalf("invalid JSON for %q: %v (%s)", c, err, got)
		}
		if out != c {
			t.Fatalf("round-trip mismatch: in %q out %q", c, out)
		}
	}
}

func TestAppendBoolNull(t *testing.T) {
	if string(AppendBool(nil, true)) != "true" {
		t.Fatal("true")
	}
	if string(AppendBool(nil, false)) != "false" {
		t.Fatal("false")
	}
	if string(AppendNull(nil)) != "null" {
		t.Fatal("null")
	}
}

func TestAppendKey(t *testing.T) {
	got := AppendKey(nil, "id")
	if string(got) != `"id":` {
		t.Fatalf("got %s", got)
	}
}

func BenchmarkAppendString_ASCII(b *testing.B) {
	s := "hello world this is a fairly typical short string"
	dst := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendString(dst[:0], s)
	}
}

func BenchmarkAppendString_StdJSON_ASCII(b *testing.B) {
	s := "hello world this is a fairly typical short string"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(s)
	}
}

func BenchmarkAppendString_Escapes(b *testing.B) {
	s := "line1\nline2\t\"quoted\" \\ backslash \x01 control"
	dst := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = AppendString(dst[:0], s)
	}
}
