# pg2json

A focused, high-performance Go client for PostgreSQL that does **one** thing:
turn `SELECT` results into JSON, as fast as practical, with a minimum of
copies and allocations.

This is **not** a generic driver. It is **not** `database/sql`-compatible. It
is **not** an ORM. Anything that is not a `SELECT` is rejected up front.

## Why

A common pattern in data gateways is:

    HTTP request -> SQL query -> rows -> JSON -> HTTP response

The standard Go path (`database/sql` or `pgx` + `Scan(&dst)` + `encoding/json`)
spends a lot of time materialising intermediate Go values it never needs:
`int64`s, `time.Time`s, `[]byte` copies, `map[string]any`, reflection,
quoting passes. For a *passthrough* gateway, almost none of that work is
necessary — the bytes can flow from the postgres wire format directly into a
JSON output buffer.

`pg2json` is built around that observation:

    socket -> protocol parse -> per-column encoder -> JSON byte buffer

## Status

Production-shaped. The major moving parts:

- TCP + TLS (`SSLRequest` negotiation, modes `disable / prefer / require /
  verify-ca / verify-full`).
- Authentication: trust, cleartext, MD5, **SCRAM-SHA-256** (RFC 7677,
  validated against the official test vector). Works against PostgreSQL 14+
  defaults, RDS, Cloud SQL, Supabase, Neon.
- Simple Query (`Q`) and Extended Query (`Parse`/`Describe`/`Bind`/`Execute`/
  `Sync`) with a **prepared-statement cache** keyed by SQL text. First call
  pays for `Parse + Describe`; subsequent calls go straight to `Bind +
  Execute`.
- **Binary result format** for hot types: `int2/4/8`, `float4/8`, `bool`,
  `uuid`, `json`, `jsonb`, `bytea`, `date`, `timestamp`, `timestamptz`.
  Each binary cell is decoded straight into JSON without `time.Time`,
  `*big.Int`, `encoding/hex`, or any temporary string.
- Per-shape compiled column encoder plan, built once per `RowDescription`
  and reused for every row.
- Direct JSON writer with correct RFC 8259 escaping and a 256-entry
  lookup-table fast path.
- Three output modes: array of objects, NDJSON, columnar
  (`{"columns":[…],"rows":[[…],[…]]}`).
- Streaming (`io.Writer`) and buffered (`[]byte`) APIs; the streaming path
  flushes on row boundaries when the buffer crosses the configured
  threshold (default 32 KiB).
- Connection pool (`pg2json/pool`): bounded, context-aware `Acquire`, idle
  reaper, `Discard` for poisoned connections.
- Real `CancelRequest` on a side connection when the caller's `context.Context`
  is cancelled (the wire deadline is the fallback).
- Tests for: JSON escaping, type encoders (text + binary), DataRow framing,
  SCRAM RFC vectors, pool concurrency. Integration tests against a real
  PostgreSQL gated by `PG2JSON_TEST_DSN`. Microbenchmarks for every hot
  path; end-to-end harness in `cmd/pg2json_bench`.

## Quick start

```go
cfg, _ := pg2json.ParseDSN("postgres://user:pass@host:5432/db?sslmode=require")
c, err := pg2json.Open(context.Background(), cfg)
if err != nil { log.Fatal(err) }
defer c.Close()

// Buffered: returns a freshly allocated []byte
buf, err := c.QueryJSON(ctx, "SELECT id, name FROM users WHERE id = $1", 42)
// buf == [{"id":42,"name":"alice"}]

// Streaming: writes directly to the http.ResponseWriter
err = c.StreamNDJSON(ctx, w, "SELECT id, name FROM users")
```

With the pool:

```go
p, _ := pool.New(pool.Config{
    Config:   cfg,
    MaxConns: 32,
})
defer p.Close()

c, err := p.Acquire(ctx)
if err != nil { return err }
defer c.Release()
return c.StreamNDJSON(ctx, w, sql)
```

## Performance

These numbers are from a single MacBook running PostgreSQL 17 in Docker on
loopback — they are *illustrative*, not a benchmark suite. Run
`cmd/pg2json_bench` and the `tests/` benchmarks on your hardware before
making decisions.

End-to-end, 100,000 rows, NDJSON to `io.Discard`:

| scenario      | rows/s  | MB/s  | ms/op |
|---------------|---------|-------|-------|
| narrow int    | 1.51 M  | 18.6  | 66    |
| narrow text   | 1.64 M  | 32.6  | 61    |
| mixed 5-col   | 1.47 M  | 122.7 | 68    |
| wide jsonb    | 1.65 M  | 80.3  | 60    |

5,000-row mixed scenario, comparing pg2json's direct path against the
classic "scan into map + `json.Marshal`" pattern (both end at `io.Discard`):

| path                    | ns/op       | B/op        | allocs/op |
|-------------------------|-------------|-------------|-----------|
| pg2json NDJSON stream   | 4,027,914   | 82,233      | **5**     |
| pg2json buffered []byte | 3,668,842   | 430,980     | **3**     |
| scan + map + json.Marshal | 13,176,680 | 10,738,890 | 185,051   |

3.6× faster, ~130× less garbage, ~37,000× fewer allocations. The "5 allocs"
number is the actual Go heap allocations end-to-end, not per-row — the
hot-loop encoders themselves allocate **zero** bytes per cell.

Microbenchmarks (Apple M4):

| benchmark                          | ns/op  | B/op | allocs |
|------------------------------------|--------|------|--------|
| `AppendString_ASCII` (pg2json)     | 81     | 0    | 0      |
| `json.Marshal(string)` (stdlib)    | 338    | 80   | 1      |
| `EncodeInt` text                   | 22     | 0    | 0      |
| `BinaryInt8` (raw 8 bytes -> JSON) | ~12    | 0    | 0      |
| `BinaryUUID` (raw 16 bytes -> JSON)| ~25    | 0    | 0      |
| `RowEncodeMixed` (6 cols)          | 248    | 19   | 0      |

## Supported types and JSON mapping

| Postgres type        | Wire    | JSON form                  | Notes                              |
|----------------------|---------|----------------------------|------------------------------------|
| bool                 | bin/txt | `true` / `false`           |                                    |
| int2, int4, int8     | bin/txt | raw number                 | binary: `binary.BigEndian` + `strconv.AppendInt` |
| float4, float8       | bin/txt | raw number                 | NaN/±Inf → JSON string             |
| numeric              | text    | raw number                 | NaN → string; passthrough text     |
| text/varchar/bpchar  | text    | quoted, escaped string     |                                    |
| name                 | text    | quoted, escaped string     |                                    |
| uuid                 | bin/txt | quoted string              | binary: hex-format direct          |
| json, jsonb          | bin/txt | raw embedded JSON          | jsonb binary strips version byte   |
| bytea                | bin/txt | quoted hex (`"\\x…"`)      |                                    |
| date                 | bin/txt | `"YYYY-MM-DD"`             | binary: int32 days since 2000-01-01 |
| timestamp            | bin/txt | `"YYYY-MM-DDTHH:MM:SS.ffffff"` | binary: int64 µs since PG epoch |
| timestamptz          | bin/txt | `"YYYY-MM-DDTHH:MM:SS.ffffffZ"` |                                |
| (anything else)      | text    | quoted, escaped string     | safe fallback                      |

`NULL` is always emitted as the JSON literal `null`.

## Limitations / honest notes

- No COPY (yet). For pure data export at the wire's theoretical maximum, a
  `COPY (SELECT …) TO STDOUT WITH (FORMAT binary)` path would beat the
  current extended-query path; it's on the roadmap.
- No array / range / interval binary decoders yet — they fall back to text
  and the JSON-string fallback. Correct, just not optimal.
- Pool is intentionally simple (LIFO + idle reaper). Production
  deployments may want a pooler in front anyway (PgBouncer in transaction
  mode is fine, but disable the statement cache via `StmtCacheSize: -1`
  because it relies on persistent server-side prepared statements).
- The MD5 path is supported for legacy installs. Deployments still using
  it should migrate to SCRAM.
- "Faster than X" claims in this README are scoped to the specific
  workload measured. Run the benchmarks on your data shape.

## Layout

```
pg2json/
├── pg2json/                 # public API
│   ├── client.go ... query.go ... stream.go ... config.go ...
│   ├── stmtcache.go         # prepared-statement cache (LRU-ish)
│   ├── ctx.go               # context cancellation -> CancelRequest
│   └── pool/                # connection pool
├── internal/
│   ├── auth/                # MD5 + SCRAM-SHA-256
│   ├── wire/                # framed reader/writer over net.Conn
│   ├── protocol/            # message codes + OIDs (mirrored from PG headers)
│   ├── rows/                # encoder plan + DataRow hot loop
│   ├── types/               # text + binary encoders, per OID
│   ├── jsonwriter/          # direct-to-[]byte JSON appender
│   ├── bufferpool/          # sync.Pool of []byte
│   └── pgerr/               # ErrorResponse decoder
├── cmd/
│   ├── pg2json_demo/        # CLI: SELECT -> JSON
│   └── pg2json_bench/       # end-to-end perf harness
├── tests/                   # integration + comparison benchmarks
├── ARCHITECTURE.md
└── ROADMAP.md
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the design rationale and
[ROADMAP.md](ROADMAP.md) for what's planned and what's deliberately out of
scope.
