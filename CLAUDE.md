# CLAUDE.md

Guidance for Claude Code (or any future agent) working in this repo. Read
this once at the start of a session — the rest of the docs assume you have.

## What this is

A focused, high-performance Go client for PostgreSQL whose **only** job is
to turn `SELECT` results into JSON with as few copies and allocations as
possible. **Not** `database/sql`-compatible. **Not** an ORM. Anything that
isn't a `SELECT` is rejected at the API.

The deployment target is a **data gateway** in front of **Citus +
PgBouncer (transaction mode)**. Every design decision should be checked
against that stack.

## What's already done (and tested against PG17 + PgBouncer 1.25)

- TCP + TLS (`SSLRequest`, modes disable/prefer/require/verify-ca/verify-full).
- Auth: trust, cleartext, MD5, **SCRAM-SHA-256** (RFC 7677 vector validated).
- Simple Query and **Extended Query with prepared-statement cache**.
- **Binary result format** for: bool, int2/4/8, float4/8, uuid, json/jsonb,
  bytea, date, timestamp, timestamptz. Each binary cell decodes straight
  to JSON without `time.Time`, `*big.Int`, `encoding/hex`, or temp strings.
- Per-shape compiled column encoder plan (built once per RowDescription).
- JSON writer with RFC 8259 escaping, 256-entry fast-path table.
- Three output modes: array, NDJSON, columnar.
- Streaming (`io.Writer`) and buffered (`[]byte`) APIs.
- Connection pool (`pg2json/pool`): bounded, context-aware Acquire,
  `MaxConnLifetime`, `PingAfterIdle` validation, `Discard` for poisoned
  conns, transparent retry up to 3× during Acquire.
- **Real `CancelRequest`** on a side connection driven by `context.Cancel`.
- **PgBouncer-txn safe**: SQLSTATE `26000` triggers transparent re-prepare
  if no bytes have been flushed yet (proven with PgBouncer pool=4, 50 clients,
  10k queries → 0 errors).
- **Header deferred until BindComplete + first DataRow**: a query that fails
  before any row writes **zero bytes** downstream. No JSON corruption.
- **Observer hook + atomic counters**: `OnQueryStart/OnQueryEnd` with
  duration, rows, bytes, error, **SQLSTATE**. Default no-op = zero overhead.
- Tests pass against direct PG **and** through PgBouncer-txn with ~1% overhead.

## Measured performance (Apple M4, PG17 in Docker on loopback)

- 1.5M rows/s sustained, 124 MB/s on 5-column mixed (id/name/score/flag/jsonb).
- vs `scan + map[string]any + json.Marshal`: **3.6× faster, ~37,000× fewer allocs**.
- Hot loop (per row): 248 ns, 0 allocs.
- PgBouncer-txn overhead vs direct: **~1%**.

## Stack assumptions baked into the code

1. **Citus**: errors can arrive mid-stream (a worker fails after coordinator
   already streamed N rows). Our contract: caller gets the error, partial
   downstream JSON is the caller's problem to discard. We document this
   and don't pretend otherwise.
2. **PgBouncer-txn**: backend rotation invalidates server-side prepared
   statements. We detect SQLSTATE `26000` and retry once with re-Parse.
3. **Server is trusted**: jsonb passthrough assumes the server sent valid
   JSON. Numeric/timestamp text-protocol passthrough validates only what's
   needed to keep the JSON output well-formed.

## Layout

```
pg2json/
├── pg2json/                 # public API
│   ├── conn.go              # Open/Close, handshake, auth dispatch, Ping, SendCancel
│   ├── config.go            # Config, SSLMode, ParseDSN
│   ├── query.go             # Query/Stream entrypoints, simple+cached extended paths
│   ├── stream.go            # outWriter abstraction (buf + flushing)
│   ├── stmtcache.go         # per-conn prepared-statement cache
│   ├── observer.go          # telemetry hook + atomic counters
│   ├── ctx.go               # ctx -> CancelRequest watcher
│   ├── args.go              # Go value -> text-format wire
│   └── pool/                # connection pool
├── internal/
│   ├── wire/                # framed reader/writer; one scratch buffer per conn
│   ├── protocol/            # message codes + OIDs (mirrored from PG headers)
│   ├── auth/                # md5, SCRAM-SHA-256
│   ├── rows/                # plan compile + DataRow hot loop
│   ├── types/               # text + binary encoders, per OID
│   ├── jsonwriter/          # direct-to-[]byte appender + escape table
│   ├── bufferpool/          # sync.Pool of []byte
│   └── pgerr/               # ErrorResponse decoder
├── cmd/
│   ├── pg2json_demo/        # CLI: SELECT -> JSON (uses PG2JSON_DSN)
│   └── pg2json_bench/       # end-to-end perf harness
├── tests/                   # integration + comparison + pgbouncer tests
├── ARCHITECTURE.md
├── ROADMAP.md
└── README.md
```

## Build / test commands you'll actually use

```bash
go build ./...
go vet ./...
go test ./...                         # unit tests only

# Integration (a Postgres at this DSN):
PG2JSON_TEST_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
  go test ./... -count=1

# PgBouncer-txn rotation test (separate DSN pointing at PgBouncer):
PG2JSON_PGBOUNCER_DSN='postgres://user:pass@host:6432/db?sslmode=disable' \
  go test ./tests/ -run TestLivePgBouncerRotation -count=1

# End-to-end perf:
PG2JSON_DSN='...' go run ./cmd/pg2json_bench -rows 100000 -repeat 5

# Comparison vs scan+map+marshal:
PG2JSON_TEST_DSN='...' go test ./tests/ -bench=BenchmarkCompare -benchmem -benchtime=3s
```

## Hard rules when editing this codebase

1. **Hot loop is sacred**. The DataRow loop in `internal/rows/datarow.go`
   and per-cell encoders in `internal/types/*.go` MUST stay zero-alloc. If
   you change them, run `go test -bench='RowEncodeMixed|EncodeInt|EncodeText' -benchmem`
   and confirm `0 allocs/op`. No exceptions.

2. **No reflection in the hot path**. Encoders are explicit functions
   selected by OID. New types add one function and one switch entry.

3. **Server is trusted only for things it documents**. JSONB binary form
   has a 1-byte version prefix; we check it. Numeric text form can be `NaN`;
   we route it to a string. Don't add "trust me" passes without a comment
   explaining why the trust is safe.

4. **Anything reachable from a goroutine other than the Client owner must
   be safe**. The Observer is shared across Clients via Pool — keep
   implementations on it lock-free or document the lock. The atomic
   counters in `observer.go` are the template.

5. **Header bytes never leave us before BindComplete**. The "no JSON
   corruption on early failure" guarantee depends on `writeHeader` happening
   only after the first DataRow OR at ReadyForQuery for empty results. Don't
   move the call earlier.

6. **PgBouncer-txn compatibility is non-negotiable**. Any change that adds
   a new round trip or new server-side state needs to survive backend
   rotation. The retry-on-26000 pattern in `runCached` is the template.

7. **Document tradeoffs in code, not in chat**. If a non-obvious choice
   was made (binary-format selector, text-passthrough validation, escape
   table size), there's a comment explaining the alternative. Keep that
   discipline.

8. **Don't add deps without a strong reason**. Today: stdlib only. SCRAM
   uses hand-rolled PBKDF2 to avoid `golang.org/x/crypto`. The bar is high.

## P1 hardening — status

Done this session:

- Hard byte/row caps (`Config.MaxResponseBytes` / `MaxResponseRows` →
  `*ResponseTooLargeError` with Committed flag; CancelRequest + drain).
- Flush by time (`Config.FlushInterval`).
- `Observer.OnQuerySlow` (`Config.SlowQueryThreshold`).
- `Observer.OnNotice` (NoticeResponse no longer dropped).
- Retry on SQLSTATE 40001 / 40P01 (`Config.RetryOnSerialization`).
- TCP keepalive on dial (`Config.Keepalive`, default 30s).
- Handshake honours ctx deadline (SetDeadline during startup/TLS/auth).
- `Pool.Drain(ctx)` and `Pool.WaitIdle(ctx)`.
- Binary decoders for arrays (recursive, multi-dim), interval, time, timetz.
- `Config.DefaultQueryTimeout` (ctx-based per-query default).

Still pending:

- Real 8h / 500k-user soak validation (shadow traffic in production).
- COPY binary fast-export path (P2).
- Numeric binary decoder (P2).

## Known gaps (P2, deliberate)

- COPY binary fast-export path.
- Numeric binary decoder (text form is fine for now).
- Connect-time protocol negotiation (3.1+/grease).

## Timeouts: defence in depth

The driver ships three knobs; none is a security boundary on its own.

  1. `postgresql.conf` `statement_timeout` — DBA-owned, final hard ceiling.
     Protects against misbehaving clients and our own bugs.
  2. `Config.DefaultQueryTimeout` — gateway-level sane default applied
     only when the caller's ctx has no deadline. Fires a real server-side
     CancelRequest via the existing ctx watcher.
  3. `ctx.WithTimeout` at the HTTP handler — per-request override that
     always wins if shorter than (2).

Do NOT rely on the client-side timeouts as a security boundary. If a
bug in pg2json or a misbehaving caller leaks a ctx without deadline,
only (1) stops a runaway query from burning a backend for hours.

## Things this project will NOT do

- INSERT/UPDATE/DELETE (rejected at the API).
- LISTEN/NOTIFY.
- Replication protocol.
- ORM features. Struct mapping. `database/sql` adapter.
- Generic "scan into anything" path.

If the user asks for any of these, push back: this codebase wins by focus.

## Workflow rules for Claude when editing here

1. **Always run `go vet ./... && go test ./...` after a change.** If you
   touched anything reachable from the wire layer, also run integration
   tests against the live PG (DSN above).
2. **Never break the zero-alloc invariants.** Re-run the hot-path benches.
3. **Commits**: small, focused, descriptive. Don't bundle unrelated changes.
   Don't commit unless asked. Ask before pushing.
4. **The user values honesty**. Don't claim "fastest" or "perfect". Run
   the benchmarks and report numbers. If something is a TODO, mark it as
   one — don't pretend it works.
5. **The user prefers terse responses.** Don't summarise what the diff
   already shows. Don't write multi-paragraph "next steps" sections
   unsolicited.

## Useful context from prior sessions

- The test PG instance is `pgopt-db` container on port 55432, db `pgopt`,
  user/pass `pgopt`/`pgopt`.
- A PgBouncer-txn instance is `pgbouncer-test` on port 56432 against the
  same DB. `default_pool_size = 4` to force backend rotation.
- All performance numbers in README.md are from this hardware (Apple M4).
- The code lived briefly at `/Users/arturoeanton/github.com/postgres/postgres/go_driver_pg2json/`
  before moving here. Its bench/test results from that location apply
  unchanged.
