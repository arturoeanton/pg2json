package tests

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/arturoeanton/pg2json/pg2json"
)

func TestIteratorBasic(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	it, err := c.RawQuery(context.Background(),
		"SELECT id, name FROM bench_mixed_5col ORDER BY id LIMIT 5")
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()

	count := 0
	for {
		body, err := it.NextRaw()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(body) < 6 {
			t.Fatalf("row %d too short: %d bytes", count, len(body))
		}
		count++
	}
	if count != 5 {
		t.Fatalf("got %d rows, want 5", count)
	}
}

func TestIteratorEarlyClose(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	// Open, read 2 rows, close early. Connection must still work after.
	it, err := c.RawQuery(context.Background(),
		"SELECT id FROM bench_mixed_5col ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := it.NextRaw(); err != nil {
			t.Fatalf("next %d: %v", i, err)
		}
	}
	if err := it.Close(); err != nil {
		t.Fatalf("early close: %v", err)
	}

	// Second query on the same Client must work.
	it2, err := c.RawQuery(context.Background(),
		"SELECT id FROM bench_mixed_5col ORDER BY id LIMIT 3")
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	defer it2.Close()
	count := 0
	for {
		_, err := it2.NextRaw()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reuse next: %v", err)
		}
		count++
	}
	if count != 3 {
		t.Fatalf("reuse got %d rows, want 3", count)
	}
}

func TestIteratorFullDrain(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	it, err := c.RawQuery(context.Background(),
		"SELECT id FROM bench_mixed_5col ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()

	count := 0
	for {
		_, err := it.NextRaw()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 100000 {
		t.Fatalf("got %d, want 100000", count)
	}
	// Close after full drain must be a no-op.
	if err := it.Close(); err != nil {
		t.Fatalf("close after drain: %v", err)
	}
}

// Guard against a trivial type check: iterator surfaces Plan() after
// at least one read.
func TestIteratorPlan(t *testing.T) {
	dsn := os.Getenv("PG2JSON_TEST_DSN")
	if dsn == "" {
		t.Skip("PG2JSON_TEST_DSN not set")
	}
	c := openClient(t)
	defer c.Close()

	it, err := c.RawQuery(context.Background(), "SELECT 1::int4, 'x'::text")
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	_, err = it.NextRaw()
	if err != nil {
		t.Fatal(err)
	}
	p := it.Plan()
	if p == nil || len(p.Columns) != 2 {
		t.Fatalf("plan: %+v", p)
	}
	_ = pg2json.ModeArray // keep import used
}
