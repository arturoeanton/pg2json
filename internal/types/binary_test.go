package types

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/arturoeanton/pg2json/internal/protocol"
)

func be64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}
func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}
func be16(v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return b[:]
}

func TestBinaryInt(t *testing.T) {
	v16 := int16(-12345)
	v32 := int32(-2147483648)
	v64 := int64(math.MaxInt64)
	cases := []struct {
		oid  protocol.OID
		raw  []byte
		want string
	}{
		{protocol.OIDInt2, be16(uint16(v16)), "-12345"},
		{protocol.OIDInt4, be32(uint32(v32)), "-2147483648"},
		{protocol.OIDInt8, be64(uint64(v64)), "9223372036854775807"},
		{protocol.OIDBool, []byte{1}, "true"},
		{protocol.OIDBool, []byte{0}, "false"},
	}
	for _, c := range cases {
		enc := PickBinary(c.oid)
		if enc == nil {
			t.Fatalf("no binary encoder for oid %d", c.oid)
		}
		got := string(enc(nil, c.raw))
		if got != c.want {
			t.Fatalf("oid %d: got %s want %s", c.oid, got, c.want)
		}
	}
}

func TestBinaryFloat(t *testing.T) {
	enc := PickBinary(protocol.OIDFloat8)
	got := string(enc(nil, be64(math.Float64bits(3.14))))
	if got != "3.14" {
		t.Fatalf("got %s", got)
	}
	got = string(enc(nil, be64(math.Float64bits(math.NaN()))))
	if got != `"NaN"` {
		t.Fatalf("got %s", got)
	}
}

func TestBinaryUUID(t *testing.T) {
	enc := PickBinary(protocol.OIDUUID)
	raw := []byte{
		0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4,
		0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00,
	}
	got := string(enc(nil, raw))
	want := `"550e8400-e29b-41d4-a716-446655440000"`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestBinaryTimestamptz(t *testing.T) {
	enc := PickBinary(protocol.OIDTimestampTZ)
	// 2025-04-15 12:34:56.789012 UTC.
	want := time.Date(2025, 4, 15, 12, 34, 56, 789012000, time.UTC)
	usec := want.Unix()*1_000_000 + int64(want.Nanosecond()/1000)
	pgUsec := usec - pgEpochUnix*1_000_000
	got := string(enc(nil, be64(uint64(pgUsec))))
	expect := `"2025-04-15T12:34:56.789012Z"`
	if got != expect {
		t.Fatalf("got %s want %s", got, expect)
	}
	// must be valid JSON
	var s string
	if err := json.Unmarshal([]byte(got), &s); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryDate(t *testing.T) {
	enc := PickBinary(protocol.OIDDate)
	// 2024-02-29 (leap day) is 8825 days after 2000-01-01.
	got := string(enc(nil, be32(uint32(int32(8825)))))
	if got != `"2024-02-29"` {
		t.Fatalf("got %s", got)
	}
}

func TestBinaryJSONB(t *testing.T) {
	enc := PickBinary(protocol.OIDJSONB)
	raw := append([]byte{1}, []byte(`{"a":1}`)...)
	got := string(enc(nil, raw))
	if got != `{"a":1}` {
		t.Fatalf("got %s", got)
	}
}

func BenchmarkBinaryInt8(b *testing.B) {
	enc := PickBinary(protocol.OIDInt8)
	raw := be64(uint64(int64(1234567890)))
	dst := make([]byte, 0, 32)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = enc(dst[:0], raw)
	}
}

func BenchmarkBinaryUUID(b *testing.B) {
	enc := PickBinary(protocol.OIDUUID)
	raw := []byte{
		0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4,
		0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00,
	}
	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = enc(dst[:0], raw)
	}
}

func BenchmarkBinaryTimestamptz(b *testing.B) {
	enc := PickBinary(protocol.OIDTimestampTZ)
	raw := be64(uint64(int64(799545296000000)))
	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst = enc(dst[:0], raw)
	}
}
