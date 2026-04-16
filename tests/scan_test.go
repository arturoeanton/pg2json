package tests

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/arturoeanton/pg2json/pg2json"
)

func TestScanStructLive(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Id    int32
		Name  string
		Score float64
		Flag  bool
		Meta  json.RawMessage
	}
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT id, name, score, flag, meta FROM bench_mixed_5col ORDER BY id LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d rows, want 3", len(out))
	}
	if out[0].Id != 1 || out[0].Name != "name-1" {
		t.Fatalf("row[0]: %+v", out[0])
	}
	if out[1].Id != 2 || out[1].Flag != true {
		t.Fatalf("row[1]: %+v", out[1])
	}
	if len(out[0].Meta) == 0 {
		t.Fatalf("meta empty")
	}
}

func TestScanStructNullable(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Id int32
		V  *string
	}
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT id, v FROM bench_null_heavy ORDER BY id LIMIT 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d rows", len(out))
	}
	// id=1: odd, v='row-1'. id=2: even, v=NULL. id=3: 'row-3'. id=4: NULL.
	if out[0].V == nil || *out[0].V != "row-1" {
		t.Fatalf("row[0].V: %v", out[0].V)
	}
	if out[1].V != nil {
		t.Fatalf("row[1].V should be nil, got %q", *out[1].V)
	}
	if out[2].V == nil || *out[2].V != "row-3" {
		t.Fatalf("row[2].V: %v", out[2].V)
	}
	if out[3].V != nil {
		t.Fatalf("row[3].V should be nil")
	}
}

func TestScanStructSQLNull(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Id int32
		V  sql.NullString
	}
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT id, v FROM bench_null_heavy ORDER BY id LIMIT 4")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d rows", len(out))
	}
	// id=1: 'row-1' (Valid=true). id=2: NULL (Valid=false). id=3: 'row-3'. id=4: NULL.
	if !out[0].V.Valid || out[0].V.String != "row-1" {
		t.Fatalf("row[0].V: %+v", out[0].V)
	}
	if out[1].V.Valid {
		t.Fatalf("row[1].V should be invalid, got %q", out[1].V.String)
	}
	if !out[2].V.Valid || out[2].V.String != "row-3" {
		t.Fatalf("row[2].V: %+v", out[2].V)
	}
	if out[3].V.Valid {
		t.Fatalf("row[3].V should be invalid")
	}
}

// prefixedString is a custom type whose *T implements sql.Scanner.
// Used to verify that ScanStruct routes cells through the Scanner
// contract for user-defined types.
type prefixedString struct{ S string }

func (p *prefixedString) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		p.S = ""
		return nil
	case string:
		p.S = "sc:" + x
		return nil
	case []byte:
		p.S = "sc:" + string(x)
		return nil
	default:
		return fmt.Errorf("prefixedString: unexpected %T", v)
	}
}

// Compile-time check: *prefixedString satisfies sql.Scanner and
// driver.Valuer is not required here.
var _ sql.Scanner = (*prefixedString)(nil)
var _ driver.Valuer = (*stubValuer)(nil)

type stubValuer struct{}

func (stubValuer) Value() (driver.Value, error) { return nil, nil }

func TestScanStructCustomScanner(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Id   int32
		Name prefixedString
	}
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT id, name FROM bench_mixed_5col ORDER BY id LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	if out[0].Name.S != "sc:name-1" || out[1].Name.S != "sc:name-2" {
		t.Fatalf("names: %+v / %+v", out[0].Name, out[1].Name)
	}
}

// Ensure json.RawMessage already works — it implements sql.Scanner in
// some versions and is a common field type.
var _ = (*json.RawMessage)(nil)

func TestScanStructArray1D(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Id  int32
		Arr []int32
	}
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT id, arr FROM bench_array_int ORDER BY id LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d rows", len(out))
	}
	// init.sql seeds arr = [g, g+1, ..., g+9] so row 1 is [1..10].
	if len(out[0].Arr) != 10 || out[0].Arr[0] != 1 || out[0].Arr[9] != 10 {
		t.Fatalf("row[0].Arr: %v", out[0].Arr)
	}
	if len(out[1].Arr) != 10 || out[1].Arr[0] != 2 || out[1].Arr[9] != 11 {
		t.Fatalf("row[1].Arr: %v", out[1].Arr)
	}
}

func TestScanStructArrayMultiDimRejected(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Grid [][]int32
	}
	_, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT ARRAY[[1,2],[3,4]]::int4[] AS grid")
	if err == nil {
		t.Fatalf("expected error for multi-dim array, got nil")
	}
}
