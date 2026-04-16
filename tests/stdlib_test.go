package tests

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/arturoeanton/pg2json/pg2json/stdlib"
)

func openStdlibDB(t testing.TB) *sql.DB {
	t.Helper()
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	db, err := sql.Open("pg2json", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestStdlibQueryBasic(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	rs, err := db.Query("SELECT id, name FROM bench_mixed_5col ORDER BY id LIMIT 3")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	var ids []int32
	var names []string
	for rs.Next() {
		var id int32
		var name string
		if err := rs.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		names = append(names, name)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 1 {
		t.Fatalf("ids: %v", ids)
	}
	if names[0] != "name-1" || names[2] != "name-3" {
		t.Fatalf("names: %v", names)
	}
}

func TestStdlibQueryRow(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	var id int32
	var name string
	row := db.QueryRow("SELECT id, name FROM bench_mixed_5col WHERE id = $1", 42)
	if err := row.Scan(&id, &name); err != nil {
		t.Fatal(err)
	}
	if id != 42 || name != "name-42" {
		t.Fatalf("got id=%d name=%q", id, name)
	}
}

func TestStdlibQueryNullable(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	rs, err := db.Query("SELECT id, v FROM bench_null_heavy ORDER BY id LIMIT 4")
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()

	var vals []sql.NullString
	for rs.Next() {
		var id int32
		var v sql.NullString
		if err := rs.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		vals = append(vals, v)
	}
	// id=1: 'row-1'  id=2: NULL  id=3: 'row-3'  id=4: NULL
	if !vals[0].Valid || vals[0].String != "row-1" {
		t.Fatalf("row[0]: %+v", vals[0])
	}
	if vals[1].Valid {
		t.Fatalf("row[1] should be NULL, got %q", vals[1].String)
	}
	if !vals[2].Valid || vals[2].String != "row-3" {
		t.Fatalf("row[2]: %+v", vals[2])
	}
	if vals[3].Valid {
		t.Fatalf("row[3] should be NULL")
	}
}

func TestStdlibQueryTime(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	var ts time.Time
	if err := db.QueryRow(
		"SELECT '2026-04-16 12:34:56+00'::timestamptz",
	).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts.Year() != 2026 || ts.Month() != time.April || ts.Day() != 16 {
		t.Fatalf("ts: %v", ts)
	}
}

func TestStdlibPrepare(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	stmt, err := db.Prepare("SELECT id FROM bench_mixed_5col WHERE id = $1")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	for _, want := range []int32{10, 20, 30} {
		var got int32
		if err := stmt.QueryRow(want).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %d want %d", got, want)
		}
	}
}

func TestStdlibExecRejected(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	_, err := db.Exec("CREATE TABLE should_not_happen (id int)")
	if err == nil {
		t.Fatal("expected error on Exec")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected 'read-only' in error, got %v", err)
	}
}

func TestStdlibBeginRejected(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	_, err := db.Begin()
	if err == nil {
		t.Fatal("expected error on Begin")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected 'read-only' in error, got %v", err)
	}
}

func TestStdlibReuseConn(t *testing.T) {
	db := openStdlibDB(t)
	defer db.Close()

	// Run the same query twice from the same pool so the statement
	// cache kicks in and the iterator must drain cleanly between calls.
	for i := 0; i < 2; i++ {
		rs, err := db.Query(
			"SELECT id FROM bench_mixed_5col ORDER BY id LIMIT $1", 3)
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		count := 0
		for rs.Next() {
			var id int32
			if err := rs.Scan(&id); err != nil {
				t.Fatal(err)
			}
			count++
		}
		rs.Close()
		if count != 3 {
			t.Fatalf("iter %d got %d rows", i, count)
		}
	}
}
