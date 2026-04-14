package types

import (
	"encoding/json"
	"testing"

	"github.com/arturoeanton/pg2json/internal/protocol"
)

func TestPickEncoders(t *testing.T) {
	cases := []struct {
		name string
		oid  protocol.OID
		raw  string
		want string // valid JSON snippet
	}{
		{"bool true", protocol.OIDBool, "t", "true"},
		{"bool false", protocol.OIDBool, "f", "false"},
		{"int4", protocol.OIDInt4, "12345", "12345"},
		{"int8 neg", protocol.OIDInt8, "-9223372036854775808", "-9223372036854775808"},
		{"float", protocol.OIDFloat8, "3.14", "3.14"},
		{"float NaN", protocol.OIDFloat8, "NaN", `"NaN"`},
		{"numeric", protocol.OIDNumeric, "1234567890.1234567890", "1234567890.1234567890"},
		{"text", protocol.OIDText, "hello\nworld", `"hello\nworld"`},
		{"json", protocol.OIDJSON, `{"a":1}`, `{"a":1}`},
		{"jsonb", protocol.OIDJSONB, `[1,2,3]`, `[1,2,3]`},
		{"uuid", protocol.OIDUUID, "550e8400-e29b-41d4-a716-446655440000",
			`"550e8400-e29b-41d4-a716-446655440000"`},
		{"bytea hex", protocol.OIDBytea, `\xdeadbeef`, `"\\xdeadbeef"`},
		{"timestamptz", protocol.OIDTimestampTZ, "2025-01-02 03:04:05+00",
			`"2025-01-02 03:04:05+00"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := Pick(c.oid)
			got := enc(nil, []byte(c.raw))
			if string(got) != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
			// And it must be valid JSON when wrapped.
			wrapped := append([]byte("["), got...)
			wrapped = append(wrapped, ']')
			var v any
			if err := json.Unmarshal(wrapped, &v); err != nil {
				t.Fatalf("invalid JSON: %v (%s)", err, wrapped)
			}
		})
	}
}

func TestNullPath(t *testing.T) {
	for _, oid := range []protocol.OID{
		protocol.OIDBool, protocol.OIDInt4, protocol.OIDFloat8,
		protocol.OIDText, protocol.OIDJSON, protocol.OIDUUID, protocol.OIDBytea,
		protocol.OIDTimestampTZ, protocol.OIDNumeric,
	} {
		got := Pick(oid)(nil, nil)
		if string(got) != "null" {
			t.Fatalf("oid %d: %s", oid, got)
		}
	}
}

func BenchmarkEncodeInt(b *testing.B) {
	enc := Pick(protocol.OIDInt8)
	raw := []byte("1234567890")
	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = enc(dst[:0], raw)
	}
}

func BenchmarkEncodeText(b *testing.B) {
	enc := Pick(protocol.OIDText)
	raw := []byte("a fairly typical row of textual data")
	dst := make([]byte, 0, 128)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = enc(dst[:0], raw)
	}
}

func BenchmarkEncodeJSONPassthrough(b *testing.B) {
	enc := Pick(protocol.OIDJSONB)
	raw := []byte(`{"id":1,"name":"alice","tags":["a","b","c"]}`)
	dst := make([]byte, 0, 128)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = enc(dst[:0], raw)
	}
}
