# pg2json

A focused PostgreSQL **read-path** driver for Go. Turns a `SELECT` into
any of these, with minimal allocations and no intermediate Go types:

- JSON bytes (array / NDJSON / columnar / TOON)
- Typed Go structs (`ScanStruct[T]`)
- `database/sql`-compatible rows (drop-in for existing code)
- Memory-bounded batched callbacks (`ScanStructBatched[T]`)

Writes, transactions, LISTEN/NOTIFY, COPY, replication — **out of
scope**. If your app also does those, keep your current driver for
them; pg2json runs alongside cleanly.

Available in [English](README.md) · [Español](README.es.md).

---

## Install

```bash
go get github.com/arturoeanton/pg2json
```

Requires Go 1.21+. No cgo. No non-stdlib dependencies in the core
library.

---

## Two API paths — pick the one that fits

pg2json exposes the same decoder through two front-ends. You can mix
them in the same program.

### Native API (fastest, read-optimised)

```go
import (
    "context"
    "database/sql"
    "encoding/json"

    "github.com/arturoeanton/pg2json/pg2json"
)

cfg, _ := pg2json.ParseDSN("postgres://user:pass@host/db?sslmode=require")
c, err := pg2json.Open(context.Background(), cfg)
if err != nil { log.Fatal(err) }
defer c.Close()

// 1) JSON bytes straight out — buffered
buf, err := c.QueryJSON(ctx,
    "SELECT id, name, meta FROM users WHERE id = $1", 42)
// buf == [{"id":42,"name":"alice","meta":{"k":42}}]

// 2) JSON streamed to any io.Writer
err = c.StreamNDJSON(ctx, w,
    "SELECT id, name FROM users WHERE active = $1", true)

// 3) Typed struct scan
type User struct {
    ID    int32
    Name  string
    Email sql.NullString       // nullable column
    Tags  []string              // text[]
    Meta  json.RawMessage       // jsonb
}
users, err := pg2json.ScanStruct[User](c, ctx,
    "SELECT id, name, email, tags, meta FROM users")

// 4) Memory-bounded batched scan — O(batchSize) memory for 100M rows
err = pg2json.ScanStructBatched[User](c, ctx, /*batch*/ 10_000,
    func(batch []User) error {
        return processUserBatch(batch) // called until all rows consumed
    },
    "SELECT id, name, email, tags, meta FROM user_log")
```

### `database/sql` adapter (drop-in for existing code)

```go
import (
    "database/sql"
    _ "github.com/arturoeanton/pg2json/pg2json/stdlib" // registers "pg2json"
)

db, err := sql.Open("pg2json",
    "postgres://user:pass@host/db?sslmode=require")
if err != nil { log.Fatal(err) }
defer db.Close()

rows, err := db.Query(
    "SELECT id, name, email FROM users WHERE active = $1", true)
for rows.Next() {
    var id int32
    var name string
    var email sql.NullString
    rows.Scan(&id, &name, &email)
    // ...
}
rows.Close()
```

The adapter is **read-only**: `db.Exec`, `db.Begin`, `db.BeginTx`
return an explicit read-only error. Do writes through your existing
driver — a second `*sql.DB` pool to the same PostgreSQL coexists
without interfering.

---

## Output modes (native only)

| method | shape | typical consumer |
|---|---|---|
| `QueryJSON(ctx, sql, args...) ([]byte, error)` | `[{...},{...}]` buffered | REST body under ~10 MB |
| `StreamJSON(ctx, w, sql, args...)` | `[{...},{...}]` streamed | HTTP response, large result |
| `StreamNDJSON(ctx, w, sql, args...)` | `{...}\n{...}\n` | line-oriented consumers (jq, Kafka, S3 NDJSON) |
| `StreamColumnar(ctx, w, sql, args...)` | `{"columns":[...],"rows":[[...],[...]]}` | grids, spreadsheets, `ag-grid`-style UIs |
| `StreamTOON(ctx, w, sql, args...)` | `[?]{col,col}\nval,val\n` | LLM / agent pipelines on a token budget |

Each mode flushes incrementally on two triggers: byte threshold
(`Config.FlushBytes`, default 32 KiB) and elapsed time
(`Config.FlushInterval`, 0 = disabled). The header is **deferred until
the first DataRow**, so a query that fails before any row produces
zero bytes downstream.

---

## Supported types

Binary format is requested for every OID that has a specialised
decoder; text is the correctness-preserving fallback.

| PostgreSQL | Wire | JSON form | Struct target |
|---|---|---|---|
| bool | binary | `true` / `false` | `bool` |
| int2, int4, int8, oid | binary | number | `int16/32/64/int`, `uint32` (OID) |
| float4, float8 | binary | number (NaN → `"NaN"`) | `float32/64` |
| numeric | text | number | `string` (or `sql.Scanner`) |
| text, varchar, bpchar, name | text | escaped string | `string`, `[]byte` |
| uuid | binary | `"xxxxxxxx-..."` | `string`, `[16]byte` |
| json, jsonb | binary | embedded JSON | `json.RawMessage`, `[]byte`, `string` |
| bytea | binary | `"\\x<hex>"` | `[]byte` |
| date, time, timetz | binary | ISO string | `time.Time` |
| timestamp, timestamptz | binary | ISO 8601 string | `time.Time` |
| interval | binary | ISO 8601 duration | `string` |
| arrays (1-D) | binary | nested JSON arrays | `[]T` of the above |
| anything else | text | escaped string (correct) | `string` |

`NULL` is emitted as JSON `null` or, in struct scan, zero-value / nil
pointer / `sql.Null*.Valid=false`. `sql.Scanner` custom types are
routed through `Scan(any)` with the `database/sql` canonical
intermediate — `uuid.UUID`, `decimal.Decimal`, and your domain types
all work without pg2json-specific wiring.

Explicitly not (yet) supported in struct scan: composite types
(`ROW(a,b)`), range types (`int4range`), multi-dim arrays,
`pgtype.*` wrappers, embedded structs. The JSON paths handle all of
these via text fallback; use the native API for now if you need them
in struct form.

---

## Performance

Apple M4 Max, PostgreSQL 17 in Docker on loopback, 100 000-row
queries. Medians of three runs. Full bench source in
`tests/full_compare_test.go`.

Shape: `bench_mixed_5col` — `id int4, name text, score float8, flag
bool, meta jsonb`.

| path | ns/op | allocs/op | bytes/op |
|---|---|---|---|
| **pg2json** | | | |
| `StreamNDJSON` | 32.0 ms | **5** | **82 KB** |
| `StreamTOON` | 31.8 ms | 5 | 82 KB |
| `StreamColumnar` | 32.0 ms | 6 | 82 KB |
| `QueryJSON` (buffered) | 32.0 ms | 30 | 53 MB |
| `ScanStruct[T]` | 31.2 ms | 200 k | 20 MB |
| `ScanStructBatched[T]` (10 k) | 30.9 ms | 200 k | **3.8 MB** |
| `stdlib.Query` | 31.7 ms | 800 k | 12 MB |
| **Reference: other Go drivers** | | | |
| naive pgx (Map + `json.Marshal`) | 121 ms | 3.3 M | 155 MB |
| pgx `RawValues` + hand-written NDJSON | 30.5 ms | 11 | 66 KB |
| pgx `Scan(&f...)` manual | 30.3 ms | 500 k | 26 MB |
| pgx `CollectRows[ByName]` | 45 ms | 700 k | 70 MB |
| pgx `stdlib.Query` | 30.1 ms | 900 k | 13.5 MB |

Notes:
- On loopback the client-side decoder is a small fraction of total
  query time (~2 ms of ~32 ms). Differences between drivers shrink
  further on LAN / WAN where wire and server dominate.
- Zero-alloc-per-cell is enforced on the JSON hot path by a bench
  that runs on every change to `internal/rows` or `internal/types`.
- The `bytes/op` column for `ScanStructBatched` shows the memory-
  bounded behaviour: the same 100 k rows fit in a rolling 3.8 MB
  buffer regardless of total row count.

Run the bench yourself:

```bash
docker compose -f docker/docker-compose.yml up -d
export PG2JSON_TEST_DSN="postgres://pgopt:pgopt@127.0.0.1:55432/pgopt?sslmode=disable"
go test ./tests -run '^$' -bench BenchmarkFullCompare -benchmem -benchtime=2s -count=3
```

---

## Production concerns

The library was built for **data gateways** fronting Citus + PgBouncer
in transaction mode. Every hardening feature exists to handle a
specific failure mode of that stack.

```go
p, _ := pool.New(pool.Config{
    Config: pg2json.Config{
        Host: "pgbouncer", Port: 6432,
        Database: "app", User: "gateway",
        MaxResponseBytes:     16 << 20,         // 16 MB per query
        MaxResponseRows:      100_000,          // row cap per query
        DefaultQueryTimeout:  10 * time.Second, // layer-2 default
        FlushInterval:        100 * time.Millisecond,
        SlowQueryThreshold:   250 * time.Millisecond,
        RetryOnSerialization: true,             // Citus rebalance
        Keepalive:            30 * time.Second,
    },
    MaxConns:           32,
    MaxInFlightBuffers: 256,
})
defer p.Close()
```

Key features for production:

- **Hard response caps** (`MaxResponseBytes` / `MaxResponseRows`)
  abort with a server-side `CancelRequest` and return a
  `*ResponseTooLargeError` that exposes whether any bytes had already
  been flushed downstream.
- **Transparent retry** on SQLSTATE 26000 (PgBouncer-txn backend
  rotation) and opt-in on 40001 / 40P01 (Citus shard rebalance).
- **Real `CancelRequest`** on `ctx.Cancel` — backends stop producing
  rows immediately, not after buffers drain.
- **Deferred header** — a query that fails before any DataRow writes
  zero bytes downstream, so HTTP responses stay clean.
- **Graceful shutdown** — `Pool.Drain(ctx)` stops accepting Acquires,
  `WaitIdle(ctx)` waits without blocking new traffic.
- **Observer** with `OnQueryStart/End/Slow/Notice` + atomic `Stats()`.
- **TCP keepalive**, socket buffer knobs (`TCPRecvBuffer`,
  `TCPSendBuffer`), configurable bufio read buffer.

---

## When to use it

| use case | fit |
|---|---|
| HTTP endpoint returning rows as JSON | native `StreamNDJSON` |
| NDJSON / S3 / Kafka export | native `StreamNDJSON` |
| Server-Sent Events, long-polling feeds | native + `FlushInterval` |
| LLM / agent feeding tabular data with a token budget | native `StreamTOON` |
| Read-path in existing `database/sql` code | adapter (one-line change) |
| Bulk read that would OOM with a plain slice | `ScanStructBatched[T]` |
| Read rows and forward them to an HTTP / queue / cache | either path |
| Writes, transactions, LISTEN/NOTIFY, COPY | keep your existing driver |
| Composite, range, multi-dim array types | keep your existing driver |

---

## Examples

### REST endpoint → NDJSON stream

```go
func listUsers(p *pool.Pool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        c, err := p.Acquire(r.Context())
        if err != nil { http.Error(w, err.Error(), 503); return }
        defer c.Release()

        w.Header().Set("Content-Type", "application/x-ndjson")
        _ = c.StreamNDJSON(r.Context(), w,
            "SELECT id, email, created_at FROM users WHERE active = $1",
            true)
    }
}
```

### Bulk export with bounded memory

```go
func exportActiveUsers(ctx context.Context, c *pg2json.Client,
    out io.Writer) error {
    return pg2json.ScanStructBatched[User](c, ctx, 5_000,
        func(batch []User) error {
            for _, u := range batch {
                b, _ := json.Marshal(u)
                if _, err := fmt.Fprintln(out, string(b)); err != nil {
                    return err
                }
            }
            return nil
        },
        "SELECT id, name, email, created_at FROM users")
}
```

### Two-pool setup for read + write

```go
var (
    reads  *pg2json.Pool  // all SELECTs
    writes *pgxpool.Pool  // INSERT / UPDATE / DELETE / transactions
)

func getUser(ctx context.Context, id int64) (User, error) {
    c, _ := reads.Acquire(ctx); defer c.Release()
    users, err := pg2json.ScanStruct[User](c, ctx,
        "SELECT id, name, email FROM users WHERE id = $1", id)
    if err != nil || len(users) == 0 {
        return User{}, err
    }
    return users[0], nil
}

func createOrder(ctx context.Context, o Order) (int64, error) {
    tx, err := writes.Begin(ctx)
    if err != nil { return 0, err }
    defer tx.Rollback(ctx)
    var id int64
    err = tx.QueryRow(ctx,
        `INSERT INTO orders (...) VALUES (...) RETURNING id`,
        o.Args()...).Scan(&id)
    if err != nil { return 0, err }
    return id, tx.Commit(ctx)
}
```

Same Postgres, two pools, clean separation of concerns.

### Drop-in for `sqlc`-generated or legacy code

```go
// Before (any driver registered as "postgres"):
db, _ := sql.Open("postgres", dsn)

// After:
import _ "github.com/arturoeanton/pg2json/pg2json/stdlib"
db, _ := sql.Open("pg2json", dsn)

// All existing db.Query / db.QueryRow / db.QueryContext / db.Prepare
// calls continue working. db.Exec / db.Begin return a clear
// read-only error, which is the signal to route writes through your
// write driver.
```

---

## Limitations — the honest list

- **Read-only by design.** INSERT / UPDATE / DELETE / LISTEN / NOTIFY
  / COPY / replication are rejected at the API or not implemented.
  Keep a write driver for those.
- **Struct scan scope.** Covers scalars, pointer-for-NULL, `sql.Null*`,
  any `sql.Scanner`, and 1-D arrays. Composite, range, multi-dim,
  `pgtype.*`, embedded structs are not in scope yet — use your write
  driver's scan path for those shapes.
- **Numeric uses text format.** Binary numeric decoder ships as an
  opt-in utility (`types.EncodeNumericBinary`) but is not
  auto-selected; on loopback the text path is faster.
- **No 8-hour / 500k-user soak yet.** Benches, fuzz tests, and
  integration suites cover correctness under bursts, but real-world
  long-duration validation is pending. Pin to `v0.x`.
- **COPY binary export is on the roadmap, not implemented.** Would
  beat the extended-query path on bulk exports where the caller can
  guarantee trusted SQL (COPY does not accept bind parameters).

---

## Layout

```
pg2json/
├── pg2json/                 # public API (native)
│   ├── conn.go query.go stream.go config.go errors.go
│   ├── scan.go              # ScanStruct[T] + ScanStructBatched[T]
│   ├── iter.go              # RawQuery / Iterator for lazy DataRow access
│   ├── stmtcache.go         # per-conn prepared-statement cache
│   ├── ctx.go               # ctx.Cancel → CancelRequest watcher
│   ├── observer.go          # telemetry hook + atomic counters
│   ├── stdlib/              # database/sql driver ("pg2json")
│   └── pool/                # connection pool + Drain / WaitIdle
├── internal/
│   ├── wire/                # framed reader/writer over net.Conn
│   ├── protocol/            # message codes + OIDs
│   ├── auth/                # MD5 + SCRAM-SHA-256 (stdlib-only)
│   ├── rows/                # plan compile + DataRow hot loop (JSON + TOON)
│   ├── types/               # text + binary encoders per OID; arrays; interval; numeric
│   ├── jsonwriter/          # direct-to-[]byte appender + escape table
│   ├── bufferpool/          # sync.Pool of []byte
│   └── pgerr/               # ErrorResponse / NoticeResponse decoder
├── docker/                  # docker-compose + seeded benchmark tables
├── cmd/
│   ├── pg2json_demo/        # CLI: SELECT → JSON (PG2JSON_DSN)
│   └── pg2json_bench/       # end-to-end perf harness
└── tests/                   # integration, comparison, scan, TOON, iter, stdlib
```

See [`ARCHITECTURE.md`](ARCHITECTURE.md) for design rationale and
[`ROADMAP.md`](ROADMAP.md) for in-flight and future work.

---

## Build tags

- `pg2json_simd` — SWAR (SIMD-Within-A-Register) implementation of
  JSON string escape. Pure Go, no assembly. Measured ~4× faster on
  medium/long ASCII strings. Off by default.

```bash
go build -tags pg2json_simd ./...
```

---

## License

See [`LICENSE`](LICENSE).
