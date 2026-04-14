# Roadmap

## Shipped

### Wire + protocol

- TCP connect with TCP keepalive (`Config.Keepalive`, default 30s).
- TLS negotiation (`SSLRequest`), modes `disable / prefer / require /
  verify-ca / verify-full`.
- Auth: trust, cleartext, MD5, **SCRAM-SHA-256** (RFC 7677 vector
  validated; stdlib-only PBKDF2).
- Startup + BackendKeyData + ParameterStatus + ReadyForQuery.
- Handshake honours the caller's `ctx.Deadline()` for the full TLS +
  auth exchange, not just the TCP dial.
- Simple Query (`Q`) and Extended Query
  (`Parse` / `Describe` / `Bind` / `Execute` / `Sync`).
- **Prepared statement cache** per connection, bounded, with
  server-side `Close` batching.
- **PgBouncer-txn safe**: SQLSTATE 26000 triggers a transparent
  re-`Parse` when no bytes have been flushed yet.
- Real `CancelRequest` on a side connection driven by
  `context.Cancel`.
- bufio-backed reader (32 KiB) — the single biggest throughput win
  for narrow-row queries (4.8× on narrow_int).

### Type decoders

- Binary format for: bool, int2/4/8, oid, float4/8, uuid, json, jsonb,
  bytea, date, timestamp, timestamptz.
- **Interval / time / timetz** binary decoders. Interval emits ISO-8601
  duration.
- **Binary array decoder**, recursive, multi-dim, dispatches through
  the scalar binary encoder by element OID. Supports int2/4/8, float,
  bool, text, varchar, uuid, json, jsonb, timestamp, timestamptz, date.
- Text-format fallback for everything else — always correct, never
  invalid JSON.
- Per-shape compiled column encoder plan, built once per
  `RowDescription`, reused for every row. Hot loop: 0 allocs/op.

### Output + streaming

- JSON output modes: array of objects, NDJSON, columnar.
- Direct JSON writer with RFC 8259 escaping and a 256-entry lookup-table
  fast path.
- Streaming `io.Writer` API flushing by **byte threshold**
  (`Config.FlushBytes`) **or by elapsed time** (`Config.FlushInterval`).
  Inline check in the row loop — no goroutine, no timer.
- Buffered `[]byte` API.
- Header is deferred until `BindComplete + first DataRow`, so a query
  that fails before any row writes zero bytes downstream.

### Hardening for Citus + PgBouncer-txn

- **Hard caps per response**: `MaxResponseBytes` / `MaxResponseRows`
  abort the query with a server-side `CancelRequest`, drain to
  `ReadyForQuery`, and return `*ResponseTooLargeError` (wraps
  `ErrResponseTooLarge`). `Committed` flag reports whether bytes had
  already been flushed downstream.
- **Retry on SQLSTATE 40001 / 40P01** (`Config.RetryOnSerialization`)
  for Citus shard rebalance. Max 3 attempts, exponential backoff from
  10ms, only when `!out.Committed()` and ctx hasn't expired.
- **`DefaultQueryTimeout`** applied when the caller's ctx has no
  deadline (layer-2 in a documented 3-layer timeout model: layer 1 is
  `postgresql.conf` `statement_timeout`; layer 3 is per-request
  `ctx.WithTimeout`).

### Observability

- `Observer` interface: `OnQueryStart`, `OnQueryEnd` (with duration,
  rows, bytes, SQLSTATE, retry count), `OnNotice` (Citus worker
  diagnostics), `OnQuerySlow` (`Config.SlowQueryThreshold`).
- Lock-free atomic `Stats()` counters (Queries, Errors, Rows, Bytes).

### Pool + lifecycle

- Bounded LIFO pool with idle reaper.
- `MaxConnLifetime` caps backend age; `PingAfterIdle` validates
  long-idle conns before handing them out.
- Transparent `Acquire` retry (up to 3×) for max-lifetime and ping
  failures.
- `Discard()` for explicit retirement of poisoned conns.
- **`Drain(ctx)`** stops accepting Acquires, closes idle immediately,
  waits for in-flight Release.
- **`WaitIdle(ctx)`** waits without blocking new Acquires.
- Per-Client `Stats()` snapshot: Open, Idle, InUse, Waiting, Max.
- **`MaxInFlightBuffers`** — optional soft cap on live scratch buffers.
  Burst over the cap bypasses `sync.Pool` rather than blocking.

### Tests

- Unit tests for JSON writer, type encoders, row plan, stream writer,
  pool concurrency, SCRAM RFC vectors.
- Integration tests against a real PostgreSQL gated by
  `PG2JSON_TEST_DSN`: types, caps, observer, handshake ctx, default
  timeouts, array / interval shapes, PgBouncer rotation.
- End-to-end harness in `cmd/pg2json_bench`.
- Full pgx comparison benchmark across 5 shapes × 3 sizes × 3 paths
  (`tests/BenchmarkPgx`).
- **Fuzz suite**: `jsonwriter` (string / bytes / key), `pgerr.Parse`,
  `rows.ParseRowDescription`. 10s × 4 runs clean (~20M+ execs per
  target). Caught and fixed a real panic on negative field counts.

---

## Still on the table

### Next

- **Shadow-traffic soak test** (8h / 500k-user). Design targets it, no
  code change needed, but real-world validation hasn't happened. Run
  this against a production canary before tagging `v1.0.0`.
- **COPY-binary fast export** (`COPY (SELECT …) TO STDOUT WITH (FORMAT
  binary)`). Skips per-row protocol overhead; the next lever for
  bulk-export workloads above the current ~165 MB/s ceiling.
- **Numeric binary decoder**. Text form works today and is
  JSON-number-shaped; binary would save one validation pass for
  numeric-heavy workloads. Reference: `numericvar_to_str` in
  `src/backend/utils/adt/numeric.c`.
- **CI**: GitHub Actions workflow with PG + PgBouncer service
  containers running the integration suite, the fuzz corpus, and the
  hot-path benchmarks as a regression gate.
- **License + v0.1.0 tag.** Blocking for anyone external consuming the
  library.
- **CHANGELOG.md** derived from the commit log.

### Later

- inet / cidr / macaddr / enum / range binary decoders — rarely
  critical for gateway workloads; text fallback is correct.
- Optional zstd / gzip writer wrapper for the streaming API.
- Connect-time protocol negotiation (3.1+ / grease) once enough
  servers expose features we want.
- `pgcopy2json` mode that uses COPY binary for absolute bulk-export
  speed.
- True SIMD JSON string escape (AVX2 on x86-64 + NEON on ARM64) via
  Go assembly with runtime dispatch. SWAR (under `pg2json_simd`) is
  the stepping stone; AVX2 would add another ~1.5-2× on the escape
  hot path, marginal on end-to-end wall time for our benchmarks, more
  noticeable on very high-QPS text-heavy gateways.
- Pipeline mode (`SendBatch`-equivalent) — multiple Parse/Bind/Execute
  groups in flight on a single connection without waiting for each
  response. Big win for gateways issuing many small queries per
  request; 0% for single-large-query workloads.

### Explicitly not planned

- INSERT / UPDATE / DELETE.
- LISTEN / NOTIFY.
- Replication protocol.
- ORM features.
- `database/sql` adapter.
- "Scan into struct" API.
- Generic "scan into anything" path.

If you need any of these, use pgx. The focus of this project is the
passthrough `SELECT → JSON` path, and keeping it narrow is what makes
the performance and correctness guarantees tractable.
