package tests

import (
	"context"
	"encoding/json"
	"testing"
)

func TestArrayInt4(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	buf, err := c.QueryJSON(context.Background(),
		"SELECT ARRAY[1,2,3]::int4[] AS a, ARRAY[]::int4[] AS e, NULL::int4[] AS n")
	if err != nil {
		t.Fatal(err)
	}
	var v []map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatalf("bad json %s: %v", buf, err)
	}
	if got, _ := json.Marshal(v[0]["a"]); string(got) != `[1,2,3]` {
		t.Fatalf("a: got %s", got)
	}
	if got, _ := json.Marshal(v[0]["e"]); string(got) != `[]` {
		t.Fatalf("e: got %s", got)
	}
	if v[0]["n"] != nil {
		t.Fatalf("n: got %v", v[0]["n"])
	}
}

func TestArrayTextWithNull(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	buf, err := c.QueryJSON(context.Background(),
		"SELECT ARRAY['hi', NULL, 'quote\"it']::text[] AS a")
	if err != nil {
		t.Fatal(err)
	}
	var v []map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatalf("bad json %s: %v", buf, err)
	}
	got := v[0]["a"].([]any)
	if len(got) != 3 || got[0] != "hi" || got[1] != nil || got[2] != `quote"it` {
		t.Fatalf("got %#v", got)
	}
}

func TestArrayInt8MultiDim(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	buf, err := c.QueryJSON(context.Background(),
		"SELECT ARRAY[ARRAY[1,2],ARRAY[3,4]]::int8[] AS a")
	if err != nil {
		t.Fatal(err)
	}
	var v []map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatalf("bad json %s: %v", buf, err)
	}
	out, _ := json.Marshal(v[0]["a"])
	if string(out) != `[[1,2],[3,4]]` {
		t.Fatalf("got %s", out)
	}
}

func TestArrayUUID(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	buf, err := c.QueryJSON(context.Background(),
		"SELECT ARRAY['11111111-1111-1111-1111-111111111111'::uuid, '22222222-2222-2222-2222-222222222222'::uuid] AS u")
	if err != nil {
		t.Fatal(err)
	}
	var v []map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatalf("bad json %s: %v", buf, err)
	}
	got := v[0]["u"].([]any)
	if got[0] != "11111111-1111-1111-1111-111111111111" ||
		got[1] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("got %#v", got)
	}
}

func TestIntervalISO(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	cases := []struct {
		sql  string
		want string
	}{
		{"INTERVAL '1 year 2 months 3 days 4 hours 5 minutes 6.789 seconds'",
			"P1Y2M3DT4H5M6.789S"},
		{"INTERVAL '0'", "PT0S"},
		{"INTERVAL '90 minutes'", "PT1H30M"},
		{"INTERVAL '15 days'", "P15D"},
	}
	for _, tc := range cases {
		buf, err := c.QueryJSON(context.Background(),
			"SELECT "+tc.sql+" AS v")
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		var v []map[string]any
		if err := json.Unmarshal(buf, &v); err != nil {
			t.Fatalf("%s: bad json %s: %v", tc.sql, buf, err)
		}
		if got := v[0]["v"]; got != tc.want {
			t.Fatalf("%s: got %v want %s", tc.sql, got, tc.want)
		}
	}
}

func TestTimeAndTimeTZ(t *testing.T) {
	c := openClient(t)
	defer c.Close()
	buf, err := c.QueryJSON(context.Background(),
		"SELECT '14:25:36.789'::time AS t, '14:25:36+03:00'::timetz AS tz, '00:00:00'::time AS z")
	if err != nil {
		t.Fatal(err)
	}
	var v []map[string]any
	if err := json.Unmarshal(buf, &v); err != nil {
		t.Fatalf("bad json %s: %v", buf, err)
	}
	if v[0]["t"] != "14:25:36.789" {
		t.Fatalf("time: got %v", v[0]["t"])
	}
	if v[0]["tz"] != "14:25:36+03:00" {
		t.Fatalf("timetz: got %v", v[0]["tz"])
	}
	if v[0]["z"] != "00:00:00" {
		t.Fatalf("time zero: got %v", v[0]["z"])
	}
}
