package rows

import (
	"testing"

	"github.com/arturoeanton/pg2json/internal/protocol"
)

func TestTOONHeaderAndRow(t *testing.T) {
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

	// Header: [?]{id,name,meta,missing}\n
	wantHdr := "[?]{id,name,meta,missing}\n"
	if string(plan.TOONHeader) != wantHdr {
		t.Fatalf("header: got %q want %q", plan.TOONHeader, wantHdr)
	}

	row := buildDataRow([][]byte{
		[]byte("42"),
		[]byte("alice\nbob"),
		[]byte(`{"k":"v"}`),
		nil,
	})
	got, err := AppendTOONRow(nil, row, plan)
	if err != nil {
		t.Fatal(err)
	}
	want := "42,\"alice\\nbob\",{\"k\":\"v\"},null\n"
	if string(got) != want {
		t.Fatalf("row: got %q\nwant %q", got, want)
	}
}

func TestTOONHeaderWithSpecialNames(t *testing.T) {
	desc := buildRowDescription([]struct {
		Name string
		OID  protocol.OID
	}{
		{"normal", protocol.OIDInt4},
		{"has,comma", protocol.OIDInt4},
		{"has space", protocol.OIDInt4},
	})
	plan, err := ParseRowDescription(desc)
	if err != nil {
		t.Fatal(err)
	}
	want := "[?]{normal,\"has,comma\",\"has space\"}\n"
	if string(plan.TOONHeader) != want {
		t.Fatalf("got %q want %q", plan.TOONHeader, want)
	}
}
