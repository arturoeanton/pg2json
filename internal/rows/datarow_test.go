package rows

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/arturoeanton/pg2json/internal/protocol"
)

// buildRowDescription constructs a synthetic 'T' body for testing.
func buildRowDescription(cols []struct {
	Name string
	OID  protocol.OID
}) []byte {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, uint16(len(cols)))
	for _, c := range cols {
		buf = append(buf, c.Name...)
		buf = append(buf, 0)
		// table OID
		buf = append(buf, 0, 0, 0, 0)
		// attrnum
		buf = append(buf, 0, 0)
		// type OID
		var oid [4]byte
		binary.BigEndian.PutUint32(oid[:], uint32(c.OID))
		buf = append(buf, oid[:]...)
		// typelen
		buf = append(buf, 0, 0)
		// typmod
		buf = append(buf, 0, 0, 0, 0)
		// format
		buf = append(buf, 0, 0)
	}
	return buf
}

func buildDataRow(cells [][]byte) []byte {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, uint16(len(cells)))
	for _, c := range cells {
		var l [4]byte
		if c == nil {
			binary.BigEndian.PutUint32(l[:], 0xFFFFFFFF)
			buf = append(buf, l[:]...)
			continue
		}
		binary.BigEndian.PutUint32(l[:], uint32(len(c)))
		buf = append(buf, l[:]...)
		buf = append(buf, c...)
	}
	return buf
}

func TestPlanAndRow(t *testing.T) {
	desc := buildRowDescription([]struct {
		Name string
		OID  protocol.OID
	}{
		{"id", protocol.OIDInt4},
		{"name", protocol.OIDText},
		{"meta", protocol.OIDJSONB},
		{"missing", protocol.OIDText},
	})
	plan, err := ParseRowDescription(desc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Columns) != 4 || plan.Columns[0].Name != "id" {
		t.Fatalf("bad plan: %+v", plan.Columns)
	}

	row := buildDataRow([][]byte{
		[]byte("42"),
		[]byte("alice\nbob"),
		[]byte(`{"k":"v"}`),
		nil,
	})
	got, err := AppendObject(nil, row, plan)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":42,"name":"alice\nbob","meta":{"k":"v"},"missing":null}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func BenchmarkRowEncodeMixed(b *testing.B) {
	desc := buildRowDescription([]struct {
		Name string
		OID  protocol.OID
	}{
		{"id", protocol.OIDInt8},
		{"name", protocol.OIDText},
		{"score", protocol.OIDFloat8},
		{"flag", protocol.OIDBool},
		{"tags", protocol.OIDJSONB},
		{"uuid", protocol.OIDUUID},
	})
	plan, _ := ParseRowDescription(desc)
	row := buildDataRow([][]byte{
		[]byte("9223372036854775807"),
		[]byte("a fairly normal name"),
		[]byte("3.14159"),
		[]byte("t"),
		[]byte(`["red","green","blue"]`),
		[]byte("550e8400-e29b-41d4-a716-446655440000"),
	})
	dst := make([]byte, 0, 256)
	b.ReportAllocs()
	b.SetBytes(int64(len(row)))
	for i := 0; i < b.N; i++ {
		dst, _ = AppendObject(dst[:0], row, plan)
	}
}
