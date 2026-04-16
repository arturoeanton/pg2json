# pg2json

**`SELECT` → JSON bytes (primary path) or `SELECT` → typed Go structs
(narrow secondary path). No intermediate `map[string]any`. Built for
dynamic data gateways where the only consumer of a row is JSON, and for
read-path code that wants structs without the pgx ceremony.**

```go
// JSON bytes — the original mission.
buf, err := c.QueryJSON(ctx, "SELECT id, name, meta FROM users WHERE id = $1", 42)
// buf == [{"id":42,"name":"alice","meta":{"k":42}}]

// Typed structs — added for SELECT-only apps that want to beat pgx on
// the read path without giving up struct ergonomics.
type User struct {
    Id    int32
    Name  string
    Meta  json.RawMessage
}
users, err := pg2json.ScanStruct[User](c, ctx,
    "SELECT id, name, meta FROM users WHERE id = ANY($1)", ids)
```

The JSON path never goes through a `struct`, a `map[string]any`, or
`json.Marshal`. Bytes flow from the Postgres wire directly into the JSON
buffer via per-OID encoders that allocate zero bytes per cell.

The struct path compiles a per-shape plan once (reflection at
RowDescription time, never per row) and fills `[]T` through
`unsafe.Pointer` + typed setters. Supported cell→field mappings cover
scalars, pointer-for-NULL, `sql.Null*`, any `sql.Scanner`, and 1-D PG
arrays. Anything beyond that (INSERT/UPDATE/DELETE, transactions,
`database/sql` compat, composite/range types, multi-dim arrays) is out
of scope by design — delegate those to pgx.

---

## When to use this — and when not to

Pick the tool that matches what you're doing with the rows in Go:

| workload                                                             | recommendation              |
|----------------------------------------------------------------------|-----------------------------|
| Read rows, run business logic in Go, write back                      | **pgx** (or `database/sql`) |
| Read rows and forward them as JSON without touching them in Go       | **pg2json** (QueryJSON / StreamNDJSON / StreamTOON) |
| Scan into typed structs of plain types + `sql.Null*` / Scanner / 1-D arrays | **pg2json.ScanStruct[T]** (1.1–1.35× vs pgx.CollectRows, 3–5× fewer allocs) |
| Scan into structs with composite types, range types, multi-dim arrays, or custom pgtype wrappers | **pgx**                     |
| Mixed INSERT/UPDATE/DELETE with SELECT inside a single transaction   | **pgx** (pg2json rejects non-SELECT at the API) |
| Feeding tabular data to an LLM or agent pipeline with a token budget | **pg2json.StreamTOON** (30–50% fewer bytes/tokens than NDJSON) |

If a single code path between the DB read and the JSON write needs the
values as Go types, the advantage disappears. Use pgx.

### Concrete fits

The common pattern is: *"the row's only consumer is JSON."* Every one of
these fits it:

- **Dynamic data gateways** fronting Citus + PgBouncer-txn for a fleet of
  internal services — the canonical target. Arbitrary SQL comes in, JSON
  goes out.
- **Read-only / read-mostly REST APIs** (`GET /resource`, `GET /search`).
  Handler is one line: parse params → `StreamNDJSON` → done.
- **NDJSON exports to data lakes / warehouses**: S3, GCS, BigQuery load
  jobs, Snowflake `COPY FROM JSON`, Databricks ingest.
- **Server-Sent Events / long-polling feeds**. `Config.FlushInterval` was
  designed for this: bytes reach the client on a predictable cadence even
  when the underlying query trickles rows.
- **Webhook payloads** — HTTP POSTs whose body is "these rows, as JSON".
- **Cache warming** — `SET key <json blob>` to Redis/Memcached/etc.,
  skipping the intermediate marshal step.
- **GraphQL list/connection resolvers** when the resolver's output shape
  mirrors the SQL columns.
- **Internal admin panels** reading arbitrary tables with a schema the
  code doesn't know at compile time.
- **Message producers (Kafka / PubSub / SQS)** with JSON payloads.
- **Golden / snapshot tests** that capture deterministic SELECT output.

### Doesn't fit

- Transactions with mixed INSERT/UPDATE/DELETE (delegate to pgx).
- ORM features, `sqlc`-style code generation (use pgx + sqlc).
- Struct fields that require `pgtype.*` wrappers, composite/range types,
  multi-dim arrays, or custom `driver.Valuer` on the write side.
- `database/sql` drop-in compatibility.
- Computed fields that require Go (use SQL expressions instead — still fits).

For any of the above, use pgx directly (or run a separate pgx pool
alongside pg2json — they coexist without interfering, both pointing at
the same PG).

---

## Target stack

**The production target is Citus + PgBouncer-txn.** Every hardening
feature in the client is justified by a specific failure mode of that
stack:

| Feature                                         | Problem it addresses                                                                                 |
|-------------------------------------------------|------------------------------------------------------------------------------------------------------|
| Retry on SQLSTATE 26000                         | PgBouncer-txn rotates backends between txns; cached prepared-statement names die. Transparent retry. |
| Retry on SQLSTATE 40001 / 40P01 (opt-in)        | Citus shard rebalance produces serialization_failure / deadlock_detected on SELECTs routinely.       |
| `Observer.OnNotice`                             | Citus surfaces worker diagnostics via `NoticeResponse`. Previously dropped.                          |
| `MaxResponseBytes` / `MaxResponseRows`          | Citus fan-out without `LIMIT` can stream GB from N workers and OOM the gateway.                      |
| Deferred header until first DataRow             | A Citus worker failing mid-plan never writes partial JSON downstream.                                |
| `FlushInterval`                                 | Citus queries with slow first-byte + trickle of worker rows still flush to HTTP clients on time.     |
| Binary array / interval / time decoders         | Citus aggregates frequently return arrays; binary decode avoids text-array parsing.                  |
| TCP keepalive + ctx-aware handshake             | Long idles behind NAT / PgBouncer no longer die silently on 8-hour sessions.                         |
| `Pool.Drain(ctx)` / `WaitIdle`                  | Rolling deploys of the gateway without killing in-flight queries.                                    |
| `DefaultQueryTimeout` + docs on layered defense | Sane gateway-level default; the hard ceiling lives in `postgresql.conf` `statement_timeout`.         |
| Real `CancelRequest` on `context.Cancel`        | Queries actually stop server-side; no zombie workers in Citus.                                       |

The client works directly against PostgreSQL too — the features above are
inert when the pain point isn't present. But the design decisions and the
performance numbers below always assume the production path goes through
PgBouncer.

---

## Performance

Apple M4, PostgreSQL 17 in Docker on loopback. NDJSON streamed to
`io.Discard`. 100,000-row result sets unless noted. Allocation counts are
**total heap allocations for the entire query end-to-end**, not per row —
the per-row hot loop allocates zero bytes.

### Via PgBouncer-txn (production target)

Comparison is against `pgx.Query` + `rows.Values()` + `map[string]any` +
`json.Marshal` — the only pgx path that works for a dynamic gateway where
the SQL and column types are unknown at compile time.

| shape       | pg2json                       | pgx + Values + map + Marshal        | speedup  | allocs ratio |
|-------------|-------------------------------|-------------------------------------|----------|--------------|
| narrow_int  | 13.1 ms, 98.4 MB/s, 6 allocs  | 33.3 ms, 38.7 MB/s, 999,805 allocs  | **2.54×** | **166,634×** |
| mixed_5col  | 57.7 ms, 151.7 MB/s, 6 allocs | 122.0 ms, 71.0 MB/s, 3,299,992 allocs | **2.11×** | **549,999×** |
| wide_jsonb  | 41.5 ms, 125.0 MB/s, 6 allocs | 137.1 ms, 32.7 MB/s, 3,399,953 allocs | **3.30×** | **566,659×** |
| array_int   | 53.7 ms, 148.5 MB/s, 6 allocs | 139.5 ms, 57.2 MB/s, 4,497,476 allocs | **2.60×** | **749,579×** |
| null_heavy  | 19.4 ms, 130.7 MB/s, 6 allocs | 43.1 ms, 58.8 MB/s, 1,299,816 allocs | **2.22×** | **216,636×** |

**Average: 2.55× faster throughput, ~450,000× fewer allocations.**

### Direct PG (reference, no PgBouncer)

| shape       | pg2json                   | pgx + Values + map + Marshal        |
|-------------|---------------------------|-------------------------------------|
| narrow_int  | 13.1 ms, 98.5 MB/s        | 33.2 ms, 38.8 MB/s                  |
| mixed_5col  | 53.5 ms, 163.8 MB/s       | 119.8 ms, 72.3 MB/s                 |
| wide_jsonb  | 38.5 ms, 134.7 MB/s       | 136.7 ms, 32.9 MB/s                 |
| array_int   | 49.5 ms, 161.1 MB/s       | 134.8 ms, 59.2 MB/s                 |
| null_heavy  | 18.4 ms, 138.0 MB/s       | 42.2 ms, 60.0 MB/s                  |

PgBouncer overhead vs direct: 1–8%, matching the historical baseline.

### Ceiling: vs hand-tuned pgx

A third path exists: `pgx.Query` with `QueryResultFormats([all binary])` +
`rows.RawValues()` + a hand-written per-OID dispatcher + a hand-written
JSON encoder. This path is **tied with pg2json** (within ±2%) on every
shape and size measured.

It's also **essentially reimplementing pg2json inline in your app**. You
have to write the OID switch, the binary decoders, the JSON escape rules,
the array-recursion, the interval/time/timestamp formatters. For a
dynamic gateway that is not something you can skip — you would not know
the column types at compile time. The ceiling is informative (it says
pg2json is at the wire-throughput limit, not leaving performance on the
table); it is not a realistic alternative to compare against.

### Output modes: NDJSON vs Columnar vs TOON (100k rows, same query)

All three write to the same `io.Writer`. NDJSON is one object per line.
Columnar is `{"columns":[...], "rows":[[...]]}`. TOON is a header line
followed by value-only rows, for LLM / agent consumers on a token
budget.

| shape       | NDJSON B/row | Columnar B/row | TOON B/row | TOON vs NDJSON |
|-------------|--------------|-----------------|------------|-----------------|
| narrow_int  | 12.9         | 7.9             | **5.9**    | **−54%**        |
| mixed_5col  | 87.6         | 53.6            | **51.6**   | **−41%**        |
| wide_jsonb  | 51.9         | 42.9            | **40.9**   | −21%            |
| array_int   | 79.8         | 68.8            | **66.8**   | −16%            |
| null_heavy  | 25.3         | 16.3            | **14.3**   | **−43%**        |

Wall time on loopback is identical across the three modes (we're
wire-bound by PG). The byte savings translate to wall time on LAN/WAN
where the wire is the bottleneck (~20–35% less transmission time for
typical row shapes at 1 Gbps).

### Struct scan (ScanStruct[T]) vs pgx — 100k rows, binary format both sides

| shape       | pg2json ScanStruct[T]           | pgx + rs.Scan(&f…) manual           | pgx.CollectByName                    |
|-------------|---------------------------------|--------------------------------------|---------------------------------------|
| narrow_int  | 7.9 ms, **34 allocs**, 1.0 MB   | —                                    | 8.7 ms, 200k allocs, 4.0 MB           |
| mixed_5col  | 32 ms, **200k allocs**, 20 MB   | 30 ms, 600k allocs, 33 MB            | 43 ms, 700k allocs, 70 MB             |
| wide_jsonb  | 34 ms, **100k allocs**, 13 MB   | —                                    | 45 ms, 700k allocs, 47 MB             |

vs `pgx.CollectRows[StructByName]` (the ergonomic pgx path): **1.1–1.35×
faster wall time, 3.5–5900× fewer allocations** depending on shape.

vs `rs.Scan(&f1, &f2, ...)` (the verbose fast pgx path): **wall-time
parity within noise, 3× fewer allocations**. Wall time is tied because
both paths are wire-bound by PG; pg2json wins on GC pressure under
high-QPS loads and on total memory footprint.

### Hot-path microbenchmarks

| benchmark                                 | ns/op | B/op | allocs |
|-------------------------------------------|-------|------|--------|
| `RowEncodeMixed` (6-col row → JSON bytes) | 57    | 0    | **0**  |
| `EncodeInt` (text)                        | 5     | 0    | **0**  |
| `EncodeText` (ASCII)                      | 21    | 0    | **0**  |

Hot loop invariant: zero allocations per cell. Enforced by bench on every
change that touches `internal/rows` or `internal/types`.

---

## Quick start

```go
cfg, _ := pg2json.ParseDSN("postgres://user:pass@host:5432/db?sslmode=require")
c, err := pg2json.Open(context.Background(), cfg)
if err != nil { log.Fatal(err) }
defer c.Close()

// Buffered: returns a freshly allocated []byte
buf, err := c.QueryJSON(ctx, "SELECT id, name FROM users WHERE id = $1", 42)

// Streaming: writes directly to an http.ResponseWriter
err = c.StreamNDJSON(ctx, w, "SELECT id, name FROM users")

// Output modes — pick based on consumer:
//   StreamJSON      → [{...},{...},...]              (array, typical REST body)
//   StreamNDJSON    → {...}\n{...}\n...              (line-delimited, Kafka / jq / tail)
//   StreamColumnar  → {"columns":[...],"rows":[...]} (AG Grid / DataTables / spreadsheet)
//   StreamTOON      → [?]{col}\nval,val\n...         (LLM agents, token-bound consumers)

// Struct scan — fill []T directly from the wire. Generic, no reflect
// per row, unsafe.Pointer setters. Supports sql.Null*, sql.Scanner,
// pointer-for-NULL, 1-D arrays. Falls fast on anything else.
type User struct {
    Id    int32
    Name  string
    Email sql.NullString
    Tags  []string          // text[] → []string
}
users, err := pg2json.ScanStruct[User](c, ctx,
    "SELECT id, name, email, tags FROM users WHERE active = true")
```

### Two API paths — pick consciously

pg2json exposes two ways to run reads. Both hit the same wire decoder.
They differ in ergonomics and performance.

**Native** (`pg2json.Client` / `pg2json.ScanStruct` / `StreamNDJSON` / `StreamTOON` / `QueryJSON`)

- Fastest path. Zero-alloc JSON hot loop, typed struct scan with
  compiled plans, streaming output modes.
- Non-standard API. Your code imports `pg2json` directly.
- **Use when**: the SELECT path is in a hot request or you want the
  full speed. New code, read-heavy gateways.

**`database/sql` adapter** (`pg2json/stdlib` registers `"pg2json"`)

```go
import (
    "database/sql"
    _ "github.com/arturoeanton/pg2json/pg2json/stdlib"
)

db, err := sql.Open("pg2json", "postgres://user:pass@host/db?sslmode=disable")
rows, err := db.Query("SELECT id, name FROM users WHERE active = $1", true)
for rows.Next() { rows.Scan(&id, &name) }
```

- Drop-in for anything using `database/sql`: sqlc-generated code,
  goose migrations (for the SELECT side), ORMs, legacy repos.
- Read-only: `db.Exec` / `db.Begin` / `db.BeginTx` return a
  read-only error. Use pgx's stdlib adapter alongside for writes —
  two `*sql.DB` pools to the same Postgres coexist cleanly.
- ~10–30% slower than the native path and 2–3× more allocs due to
  `driver.Value` interface boxing. Still faster than `pgx +
  database/sql` on the same shape.
- **Use when**: migrating an existing `database/sql` codebase, or
  when the hot path is not a read you control.

### Pool (production)

```go
p, _ := pool.New(pool.Config{
    Config: pg2json.Config{
        Host: "pgbouncer", Port: 6432,
        Database: "app", User: "gateway",
        MaxResponseBytes:     16 << 20,          // 16 MB cap per query
        MaxResponseRows:      100_000,           // row cap per query
        DefaultQueryTimeout:  10 * time.Second,  // layer-2 default; layer-1 is postgresql.conf
        FlushInterval:        100 * time.Millisecond,
        SlowQueryThreshold:   250 * time.Millisecond,
        RetryOnSerialization: true,              // enable if Citus rebalance is routine
    },
    MaxConns:            32,
    MaxInFlightBuffers:  256, // soft cap on scratch buffers; bursts bypass the pool
})
defer p.Close()

c, err := p.Acquire(ctx)
if err != nil { return err }
defer c.Release()
return c.StreamNDJSON(ctx, w, sql, args...)
```

### Graceful shutdown

```go
// On SIGTERM:
drainCtx, cancel := context.WithTimeout(ctx, 30 * time.Second)
defer cancel()
if err := p.Drain(drainCtx); err != nil { /* log; deadline hit */ }
p.Close()
```

### Observability

```go
type myObs struct{}

func (myObs) OnQueryStart(string)                      {}
func (myObs) OnQueryEnd(ev pg2json.QueryEvent)         { /* metrics */ }
func (myObs) OnNotice(n *pgerr.Error)                  { /* Citus worker warnings */ }
func (myObs) OnQuerySlow(ev pg2json.QueryEvent)        { /* alert */ }

c.SetObserver(myObs{})
```

---

## Supported types

| Postgres type                         | Wire    | JSON form                          |
|---------------------------------------|---------|------------------------------------|
| bool                                  | binary  | `true` / `false`                   |
| int2, int4, int8, oid                 | binary  | number                             |
| float4, float8                        | binary  | number (NaN / ±Inf → string)       |
| numeric                               | text    | number (NaN → string)              |
| text, varchar, bpchar, name           | text    | escaped string                     |
| uuid                                  | binary  | `"xxxxxxxx-xxxx-..."`              |
| json, jsonb                           | binary  | embedded JSON (jsonb version byte stripped) |
| bytea                                 | binary  | `"\\x<hex>"`                       |
| date                                  | binary  | `"YYYY-MM-DD"`                     |
| timestamp                             | binary  | `"YYYY-MM-DDTHH:MM:SS[.ffffff]"`   |
| timestamptz                           | binary  | `"...Z"`                           |
| time                                  | binary  | `"HH:MM:SS[.ffffff]"`              |
| timetz                                | binary  | `"HH:MM:SS[.ffffff]±HH:MM"`        |
| interval                              | binary  | ISO 8601 duration (`"P1Y2M3DT4H5M6.789S"`) |
| arrays of all of the above            | binary  | nested JSON arrays (multi-dim OK)  |
| anything else                         | text    | escaped string (safe fallback)     |

`NULL` is always emitted as JSON `null`. Binary format is requested for
every OID that has a specialised decoder; text is used only as a
correctness-preserving fallback.

---

## Examples

Copy-paste starting points for the patterns this library is built for.
All examples assume you have an initialised `*pg2json.Client` called `c`
or a `*pool.Pool` called `p`. Error handling is abbreviated for clarity;
in production wrap each call with proper logging / metrics.

### 1. REST endpoint → JSON array

```go
// GET /api/v1/users?limit=100
func listUsers(p *pool.Pool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        c, err := p.Acquire(r.Context())
        if err != nil {
            http.Error(w, err.Error(), 503)
            return
        }
        defer c.Release()

        limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
        if limit <= 0 || limit > 1000 { limit = 100 }

        w.Header().Set("Content-Type", "application/json")
        if err := c.StreamJSON(r.Context(), w,
            "SELECT id, email, created_at FROM users ORDER BY id DESC LIMIT $1",
            limit); err != nil {
            // Headers may already be flushed; we can't change status here.
            // Observer catches it.
            return
        }
    }
}
```

### 2. NDJSON export to S3 / data lake

```go
func exportToS3(ctx context.Context, p *pool.Pool, bucket, key, sql string) error {
    c, err := p.Acquire(ctx); if err != nil { return err }
    defer c.Release()

    pr, pw := io.Pipe()
    go func() {
        defer pw.Close()
        _ = c.StreamNDJSON(ctx, pw, sql)
    }()
    _, err = s3Client.PutObject(ctx, &s3.PutObjectInput{
        Bucket: &bucket, Key: &key,
        Body:   pr,
        ContentType: aws.String("application/x-ndjson"),
    })
    return err
}
```

### 3. Server-Sent Events feed

```go
// Live feed of the last N events, one per SSE "data:" line.
// FlushInterval guarantees bytes reach the browser within 200ms even if
// rows trickle.
func eventsFeed(p *pool.Pool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        flusher, _ := w.(http.Flusher)

        c, _ := p.Acquire(r.Context()); defer c.Release()

        // Wrap w so every newline becomes "data: ...\n\n"
        sw := &sseWriter{w: w, flusher: flusher}
        _ = c.StreamNDJSON(r.Context(), sw,
            "SELECT id, payload FROM events WHERE id > $1 ORDER BY id",
            lastSeenID(r))
    }
}

type sseWriter struct { w http.ResponseWriter; flusher http.Flusher }
func (s *sseWriter) Write(p []byte) (int, error) {
    _, _ = s.w.Write([]byte("data: "))
    n, _ := s.w.Write(bytes.TrimRight(p, "\n"))
    _, _ = s.w.Write([]byte("\n\n"))
    s.flusher.Flush()
    return n, nil
}
```

### 4. Webhook payload from a SQL query

```go
func fireWebhook(ctx context.Context, c *pg2json.Client, url string, args ...any) error {
    body, err := c.QueryJSON(ctx,
        `SELECT order_id, status, total_cents, currency
         FROM orders WHERE id = ANY($1::bigint[])`, args...)
    if err != nil { return err }

    req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return err }
    resp.Body.Close()
    return nil
}
```

### 5. Warm a Redis cache with a JSON blob

```go
func cacheWarm(ctx context.Context, c *pg2json.Client, rdb *redis.Client, userID int64) error {
    body, err := c.QueryJSON(ctx,
        "SELECT id, prefs, last_login FROM user_profile WHERE id = $1",
        userID)
    if err != nil { return err }
    // body is already JSON — no json.Marshal round-trip.
    return rdb.Set(ctx, fmt.Sprintf("user:%d", userID), body, 5*time.Minute).Err()
}
```

### 6. GraphQL list resolver

```go
// Resolver for type Query { orders(limit: Int): [Order!]! }
// The resolver returns a json.RawMessage; gqlgen / graphql-go both
// accept this and skip re-serialisation.
func (r *Resolver) Orders(ctx context.Context, limit *int) (json.RawMessage, error) {
    c, _ := r.pool.Acquire(ctx); defer c.Release()
    n := 50; if limit != nil { n = *limit }
    return c.QueryJSON(ctx,
        `SELECT id, status, total_cents, customer_id
         FROM orders ORDER BY created_at DESC LIMIT $1`, n)
}
```

### 7. Kafka / PubSub producer (NDJSON messages)

```go
// Each DB row becomes one Kafka message. Streaming so the producer
// starts sending before the query finishes.
func streamToKafka(ctx context.Context, c *pg2json.Client, prod *kafka.Writer, sql string) error {
    pr, pw := io.Pipe()
    go func() { defer pw.Close(); _ = c.StreamNDJSON(ctx, pw, sql) }()

    scan := bufio.NewScanner(pr)
    scan.Buffer(make([]byte, 64*1024), 8*1024*1024) // large rows
    for scan.Scan() {
        if err := prod.WriteMessages(ctx,
            kafka.Message{Value: append([]byte(nil), scan.Bytes()...)},
        ); err != nil {
            return err
        }
    }
    return scan.Err()
}
```

### 8. Admin panel — arbitrary SELECT with hard caps

```go
// Gate a user-supplied SELECT with byte/row caps so an admin can't
// accidentally exfiltrate a 10 GB table through the UI.
func adminRun(ctx context.Context, sql string) ([]byte, error) {
    cfg := baseCfg
    cfg.MaxResponseBytes = 8 << 20   // 8 MB hard cap
    cfg.MaxResponseRows  = 10_000
    cfg.DefaultQueryTimeout = 15 * time.Second

    c, err := pg2json.Open(ctx, cfg); if err != nil { return nil, err }
    defer c.Close()

    buf, err := c.QueryJSON(ctx, sql)
    var capErr *pg2json.ResponseTooLargeError
    if errors.As(err, &capErr) {
        return nil, fmt.Errorf("query too large: %s limit reached (%d)",
            capErr.Limit, capErr.LimitVal)
    }
    return buf, err
}
```

### 9. Snapshot / golden test

```go
func TestOrdersListShape(t *testing.T) {
    c := openTestClient(t) // from your test helper
    defer c.Close()

    got, err := c.QueryJSON(context.Background(),
        `SELECT id, status FROM orders WHERE id IN (1,2,3) ORDER BY id`)
    if err != nil { t.Fatal(err) }

    want := `[{"id":1,"status":"paid"},{"id":2,"status":"refunded"},{"id":3,"status":"paid"}]`
    if string(got) != want {
        t.Fatalf("shape drift\ngot:  %s\nwant: %s", got, want)
    }
}
```

### 10. Struct scan: typed results without the pgx ceremony

```go
// Feed a ranked list to the recommendation service. Plain struct
// fields + sql.Null* + 1-D array are all that's needed 90% of the
// time — use pg2json.ScanStruct and skip pgx + CollectRows entirely.
type Candidate struct {
    Id      int64
    Handle  string
    Score   float64
    Email   sql.NullString    // nullable column
    Tags    []string          // text[]
    Seen    time.Time
}

func rank(ctx context.Context, c *pg2json.Client, since time.Time) ([]Candidate, error) {
    return pg2json.ScanStruct[Candidate](c, ctx,
        `SELECT id, handle, score, email, tags, last_seen AS seen
         FROM candidates WHERE last_seen >= $1 ORDER BY score DESC LIMIT 500`,
        since)
}
```

Field matching uses a `pg2json:"col_name"` tag if present, else the
field name case-insensitively. Columns with no matching field are
skipped; fields with no matching column stay at their zero value.

For custom types (e.g. `uuid.UUID`, `decimal.Decimal`, domain wrappers),
implement `sql.Scanner` on `*T` and ScanStruct routes through it —
same contract as `database/sql`.

### 11. Delegate to pgx for writes and transactions

```go
// pg2json owns the read path; pgx owns everything else. Two pools,
// same PG, no shared state — they coexist cleanly.
var (
    readDB  *pg2json.Pool  // SELECT / reads
    writeDB *pgxpool.Pool  // INSERT / UPDATE / DELETE / BEGIN
)

// Hot read — pg2json.
func getUsers(ctx context.Context) ([]User, error) {
    c, _ := readDB.Acquire(ctx); defer c.Release()
    return pg2json.ScanStruct[User](c, ctx, `SELECT ... FROM users ...`)
}

// Write — pgx (transactions, RETURNING, pgtype, everything).
func createOrder(ctx context.Context, o Order) (int64, error) {
    tx, err := writeDB.Begin(ctx); if err != nil { return 0, err }
    defer tx.Rollback(ctx)
    var id int64
    err = tx.QueryRow(ctx,
        `INSERT INTO orders (...) VALUES (...) RETURNING id`,
        o.Args()...,
    ).Scan(&id)
    if err != nil { return 0, err }
    return id, tx.Commit(ctx)
}
```

### 12. Graceful shutdown on SIGTERM

```go
func main() {
    p, err := pool.New(poolCfg)
    if err != nil { log.Fatal(err) }

    srv := &http.Server{Addr: ":8080", Handler: mux(p)}
    go srv.ListenAndServe()

    stop := make(chan os.Signal, 1)
    signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
    <-stop

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    _ = srv.Shutdown(ctx)      // stop accepting HTTP requests
    _ = p.Drain(ctx)           // wait for in-flight queries
    _ = p.Close()
}
```

---

## Layout

```
pg2json/
├── pg2json/                 # public API
│   ├── conn.go query.go stream.go config.go errors.go
│   ├── scan.go              # ScanStruct[T] — struct scan path
│   ├── stmtcache.go         # per-connection prepared-statement cache
│   ├── ctx.go               # context cancel → CancelRequest watcher
│   ├── observer.go          # telemetry hook + atomic counters
│   └── pool/                # connection pool + Drain / WaitIdle
├── internal/
│   ├── wire/                # framed reader/writer over net.Conn (bufio)
│   ├── protocol/            # message codes + OIDs
│   ├── auth/                # MD5 + SCRAM-SHA-256 (stdlib only)
│   ├── rows/                # plan compile + DataRow hot loop (JSON + TOON)
│   ├── types/               # text + binary encoders per OID; arrays; interval; numeric
│   ├── jsonwriter/          # direct-to-[]byte appender + escape table
│   ├── bufferpool/          # sync.Pool of []byte
│   └── pgerr/               # ErrorResponse / NoticeResponse decoder
├── docker/                  # docker-compose + seeded benchmark tables
├── cmd/
│   ├── pg2json_demo/        # CLI: SELECT → JSON (PG2JSON_DSN)
│   └── pg2json_bench/       # end-to-end perf harness
└── tests/                   # integration, comparison, PgBouncer rotation, scan, TOON
```

See [ARCHITECTURE.md](ARCHITECTURE.md) and [ROADMAP.md](ROADMAP.md) for
design rationale and pending work.

---

## Build tags

- `pg2json_simd` (experimental) — enables a SWAR (SIMD-Within-A-Register)
  implementation of JSON string escaping. Pure Go, no assembly, no
  platform-specific code. Processes 8 bytes per iteration through
  bitwise tricks on `uint64`. Measured on Apple M4: **4× faster** on
  medium/long ASCII strings compared to the default scalar
  implementation; marginal on strings dominated by escape-requiring
  bytes. Similar speedup expected on x86-64. Off by default because
  the gain only materialises for text-heavy workloads; numeric
  queries see no change.

  ```bash
  go build -tags pg2json_simd ./...
  go test -tags pg2json_simd ./...
  ```

  Fuzz tests cover both paths. A real SIMD path (AVX2 / NEON
  assembly) is a possible future addition; SWAR is the pure-Go
  stepping stone.

## Honest limitations

- **No COPY binary fast-export yet.** A `COPY (SELECT ...) TO STDOUT
  WITH (FORMAT binary)` path would beat the extended-query path for
  bulk-export workloads. Not implemented; on the roadmap.
- **Numeric binary decoder ships as an opt-in utility
  (`types.EncodeNumericBinary`) but is not auto-selected.** On loopback
  and typical precisions PG's C-side text formatter beats the Go
  decoder — wire it in manually if your workload shows text numeric
  dominating.
- **ScanStruct scope is deliberately narrow.** Supports scalars,
  pointer-for-NULL, `sql.Null*`, any `sql.Scanner`, and 1-D arrays.
  Does NOT support composite types, range types, multi-dim arrays,
  `pgtype.*` wrappers, embedded structs. Hit any of those → use pgx.
- **`database/sql` adapter available but read-only.** Import
  `_ "github.com/arturoeanton/pg2json/pg2json/stdlib"` and
  `sql.Open("pg2json", dsn)`. Query / QueryContext / QueryRow / Prepare
  / Ping all work. Exec / Begin return a read-only error — use pgx
  through its stdlib adapter for writes, in a separate sql.DB pool.
- **Writes are out of scope.** INSERT/UPDATE/DELETE/LISTEN/COPY/replication
  are rejected at the API or not implemented. Delegate to pgx — two
  pools to the same PG coexist cleanly.
- **Performance claims are against the specific workload shape
  measured.** Run `cmd/pg2json_bench` and `tests/BenchmarkPgx` /
  `tests/BenchmarkScan` on your own data shapes, on your own hardware.
  Numbers move.
- **No 8-hour / 500k-user soak test yet.** Design targets that workload,
  and the benchmark suite plus fuzz tests cover correctness under
  bursts, but real-world validation is pending. Pin to `v0.x` and
  report anything that looks off.
