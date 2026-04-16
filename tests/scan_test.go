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

func TestScanStructBatched(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Id   int32
		Name string
	}
	// bench_mixed_5col has 100k rows — use batchSize 10 000 → 10 batches.
	var totalRows int
	var batchCount int
	var firstID, lastID int32
	err := pg2json.ScanStructBatched[row](c, context.Background(), 10000,
		func(batch []row) error {
			batchCount++
			if batchCount == 1 {
				firstID = batch[0].Id
			}
			lastID = batch[len(batch)-1].Id
			totalRows += len(batch)
			return nil
		},
		"SELECT id, name FROM bench_mixed_5col ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if totalRows != 100000 {
		t.Fatalf("total rows %d, want 100000", totalRows)
	}
	if batchCount != 10 {
		t.Fatalf("batches %d, want 10", batchCount)
	}
	if firstID != 1 || lastID != 100000 {
		t.Fatalf("id range [%d, %d], want [1, 100000]", firstID, lastID)
	}
}

func TestScanStructBatchedCallbackError(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct{ Id int32 }
	sentinel := fmt.Errorf("stop here")
	err := pg2json.ScanStructBatched[row](c, context.Background(), 100,
		func(batch []row) error {
			// Abort after the first batch.
			return sentinel
		},
		"SELECT id FROM bench_mixed_5col ORDER BY id")
	if err != sentinel {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	// Connection should still be usable.
	type row2 struct{ Id int32 }
	out, err := pg2json.ScanStruct[row2](c, context.Background(),
		"SELECT id FROM bench_mixed_5col ORDER BY id LIMIT 3")
	if err != nil {
		t.Fatalf("reuse after abort: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("reuse got %d rows", len(out))
	}
}

func TestScanStructEmbedded(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type base struct {
		Id int32
	}
	type rowFlat struct {
		base // embedded anonymous struct; Id should flatten to "id"
		Name string
	}
	out, err := pg2json.ScanStruct[rowFlat](c, context.Background(),
		"SELECT id, name FROM bench_mixed_5col ORDER BY id LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	if out[0].Id != 1 || out[0].Name != "name-1" {
		t.Fatalf("row[0]: %+v", out[0])
	}
	if out[1].Id != 2 || out[1].Name != "name-2" {
		t.Fatalf("row[1]: %+v", out[1])
	}
}

func TestScanStructEmbeddedOverride(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	// Both the embedded struct and the outer declare a field that
	// resolves to the "id" column. Outer wins per the documented
	// precedence rule.
	type embedded struct {
		Id int32
	}
	type rowShadow struct {
		embedded
		Id   int32 `pg2json:"id"` // outer Id wins
		Name string
	}
	out, err := pg2json.ScanStruct[rowShadow](c, context.Background(),
		"SELECT id, name FROM bench_mixed_5col ORDER BY id LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d rows", len(out))
	}
	if out[0].Id != 1 || out[0].embedded.Id != 0 {
		t.Fatalf("outer Id=%d, embedded.Id=%d (embedded should stay zero)", out[0].Id, out[0].embedded.Id)
	}
}

func TestScanStructArray2D(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Grid [][]int32
	}
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT ARRAY[[1,2,3],[4,5,6]]::int4[] AS grid")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d rows", len(out))
	}
	g := out[0].Grid
	if len(g) != 2 || len(g[0]) != 3 || len(g[1]) != 3 {
		t.Fatalf("shape: %v", g)
	}
	want := [][]int32{{1, 2, 3}, {4, 5, 6}}
	for i := range want {
		for j := range want[i] {
			if g[i][j] != want[i][j] {
				t.Fatalf("g[%d][%d] = %d, want %d", i, j, g[i][j], want[i][j])
			}
		}
	}
}

func TestScanStructRangeBytes(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		R pg2json.RangeBytes `pg2json:"r"`
	}

	// Bounded [1, 5) — lower inclusive, upper exclusive.
	out, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT '[1,5)'::int4range AS r")
	if err != nil {
		t.Fatal(err)
	}
	r := out[0].R
	if r.Empty {
		t.Fatal("expected non-empty")
	}
	if !r.LowerInclusive || r.UpperInclusive {
		t.Fatalf("inclusivity wrong: %+v", r)
	}
	if r.LowerInfinite || r.UpperInfinite {
		t.Fatalf("infinity wrong: %+v", r)
	}
	// Lower bound = 1 as int4 binary = 4 bytes.
	if len(r.Lower) != 4 || r.Lower[3] != 1 {
		t.Fatalf("lower bytes: %v", r.Lower)
	}
	if len(r.Upper) != 4 || r.Upper[3] != 5 {
		t.Fatalf("upper bytes: %v", r.Upper)
	}

	// Empty range.
	out2, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT 'empty'::int4range AS r")
	if err != nil {
		t.Fatal(err)
	}
	if !out2[0].R.Empty {
		t.Fatalf("expected empty, got %+v", out2[0].R)
	}

	// Upper infinite: [1,).
	out3, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT int4range(1, NULL) AS r")
	if err != nil {
		t.Fatal(err)
	}
	r3 := out3[0].R
	if !r3.UpperInfinite || r3.Empty || r3.LowerInfinite {
		t.Fatalf("upper-inf shape: %+v", r3)
	}
	if len(r3.Lower) != 4 || r3.Lower[3] != 1 {
		t.Fatalf("lower bytes on upper-inf: %v", r3.Lower)
	}
	if r3.Upper != nil {
		t.Fatalf("upper must be nil when infinite, got %v", r3.Upper)
	}
}

func TestScanStructArray3DRejected(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	type row struct {
		Cube [][][]int32
	}
	_, err := pg2json.ScanStruct[row](c, context.Background(),
		"SELECT ARRAY[[[1]]]::int4[] AS cube")
	if err == nil {
		t.Fatalf("expected error for 3D array, got nil")
	}
}
