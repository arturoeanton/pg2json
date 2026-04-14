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
