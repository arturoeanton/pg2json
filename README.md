# pg2json

**`SELECT` → JSON bytes. No intermediate Go types. Built for dynamic data
gateways where the only consumer of a row is JSON.**

```go
buf, err := c.QueryJSON(ctx, "SELECT id, name, meta FROM users WHERE id = $1", 42)
// buf == [{"id":42,"name":"alice","meta":{"k":42}}]
```

The row above never becomes a `struct`, a `map[string]any`, or goes through
`json.Marshal`. Bytes flow from the Postgres wire directly into a JSON
output buffer via per-OID encoders that allocate zero bytes per cell.

Anything that is not a `SELECT` is rejected at the API. This is not a
general-purpose driver, not `database/sql`-compatible, and not an ORM.

---

## When to use this — and when not to

Pick the tool that matches what you're doing with the rows in Go:

| workload                                                             | recommendation              |
|----------------------------------------------------------------------|-----------------------------|
| Read rows, run business logic in Go, write back                      | **pgx** (or `database/sql`) |
| Scan into typed structs, validate, transform, respond with JSON      | **pgx + `encoding/json`**   |
| Read rows and forward them as JSON without touching them in Go       | **pg2json**                 |
| Mixed INSERT/UPDATE/DELETE with SELECT inside a single transaction   | **pgx** (pg2json rejects non-SELECT at the API) |

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

- Authorization or filtering logic that inspects row values in Go.
- Transactions with mixed INSERT/UPDATE/DELETE.
- Struct mapping, ORM features, `sqlc`-style code generation.
- Computed fields that require Go (use SQL expressions instead — still fits).

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

| shape       | pg2json                 | pgx + Values + map + Marshal | speedup  | allocs ratio |
|-------------|-------------------------|------------------------------|----------|--------------|
| narrow_int  | 13.0 ms, 99.0 MB/s, 6 allocs | 33.2 ms, 38.8 MB/s, 999,804 allocs | **2.55×** | **166,634×** |
| mixed_5col  | 57.8 ms, 151.5 MB/s, 6 allocs | 121.8 ms, 71.1 MB/s, 3,299,986 allocs | **2.11×** | **549,998×** |
| wide_jsonb  | 40.7 ms, 127.6 MB/s, 6 allocs | 136.6 ms, 32.9 MB/s, 3,399,951 allocs | **3.36×** | **566,659×** |
| array_int   | 53.8 ms, 148.2 MB/s, 6 allocs | 139.0 ms, 57.4 MB/s, 4,497,464 allocs | **2.58×** | **749,577×** |
| null_heavy  | 20.2 ms, 125.3 MB/s, 6 allocs | 43.6 ms, 58.2 MB/s, 1,299,817 allocs | **2.16×** | **216,636×** |

**Average: 2.55× faster throughput, ~450,000× fewer allocations.**

### Direct PG (reference, no PgBouncer)

| shape       | pg2json                  | pgx + Values + map + Marshal |
|-------------|--------------------------|------------------------------|
| narrow_int  | 12.9 ms, 99.8 MB/s       | 32.9 ms, 39.2 MB/s           |
| mixed_5col  | 53.8 ms, 162.7 MB/s      | 120.5 ms, 71.9 MB/s          |
| wide_jsonb  | 38.7 ms, 134.1 MB/s      | 138.1 ms, 32.5 MB/s          |
| array_int   | 48.5 ms, 164.6 MB/s      | 128.8 ms, 62.0 MB/s          |
| null_heavy  | 18.4 ms, 137.4 MB/s      | 41.7 ms, 60.8 MB/s           |

PgBouncer overhead vs direct: 1–7%, matching the historical baseline.

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
```

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
    MaxConns: 32,
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

### 10. Graceful shutdown on SIGTERM

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
│   ├── stmtcache.go         # per-connection prepared-statement cache
│   ├── ctx.go               # context cancel → CancelRequest watcher
│   ├── observer.go          # telemetry hook + atomic counters
│   └── pool/                # connection pool + Drain / WaitIdle
├── internal/
│   ├── wire/                # framed reader/writer over net.Conn (bufio)
│   ├── protocol/            # message codes + OIDs
│   ├── auth/                # MD5 + SCRAM-SHA-256 (stdlib only)
│   ├── rows/                # plan compile + DataRow hot loop
│   ├── types/               # text + binary encoders per OID; arrays; interval
│   ├── jsonwriter/          # direct-to-[]byte appender + escape table
│   ├── bufferpool/          # sync.Pool of []byte
│   └── pgerr/               # ErrorResponse / NoticeResponse decoder
├── cmd/
│   ├── pg2json_demo/        # CLI: SELECT → JSON (PG2JSON_DSN)
│   └── pg2json_bench/       # end-to-end perf harness
└── tests/                   # integration, comparison, PgBouncer rotation
```

See [ARCHITECTURE.md](ARCHITECTURE.md) and [ROADMAP.md](ROADMAP.md) for
design rationale and pending work.

---

## Honest limitations

- **No COPY binary fast-export yet.** A `COPY (SELECT ...) TO STDOUT
  WITH (FORMAT binary)` path would beat the extended-query path for
  bulk-export workloads. Not implemented; on the roadmap.
- **Numeric is text-format only.** PG's binary numeric format is a packed
  decimal requiring non-trivial decoding; text works correctly (the PG
  text form is already JSON-number-shaped).
- **No fuzz tests yet** for the JSON writer or protocol parser. Planned.
- **Buffer pool has no global cap.** A sustained burst can grow the pool
  arbitrarily. Planned, not critical for steady-state load.
- **Performance claims are against the specific workload shape
  measured.** Run `cmd/pg2json_bench` and `tests/BenchmarkPgx` on your
  own data shapes, on your own hardware. Numbers move.
- **This is a SELECT driver.** INSERT/UPDATE/DELETE/LISTEN/COPY/replication
  are rejected at the API or not implemented. That's the point.
