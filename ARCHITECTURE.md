# Architecture

This document captures the design rationale behind pg2json. It assumes
you have read the README and want to understand why the code is
structured the way it is, why certain things are NOT done, and where
the performance wins come from.

## One-line positioning

A Postgres client that turns `SELECT` results into JSON bytes without
ever materialising rows as Go types, tuned for a **Citus + PgBouncer-txn
data-gateway** workload.

## Layers

```
+-------------------------------------------------------------------+
| pg2json (public API)                                              |
|   Client · Open/Close · Query/Stream · Config · Observer · Pool   |
+-------------------------------------------------------------------+
| rows  (encoder plan · DataRow hot loop · column metadata)         |
+-------------------------------------------------------------------+
| jsonwriter            types  (per-OID text + binary encoders)     |
|   Append* funcs       · numeric · jsonpass · bytea · times        |
|   256-entry escape    · binary (int/float/bool/uuid/json/date/ts) |
|     lookup table      · array  (recursive, multi-dim)             |
|                       · interval · time · timetz                  |
+-------------------------------------------------------------------+
| protocol (message codes, OIDs, constants mirrored from PG headers)|
+-------------------------------------------------------------------+
| wire (framed reader/writer + bufio over net.Conn + scratch buf)   |
+-------------------------------------------------------------------+
| auth (MD5 · SCRAM-SHA-256 RFC 7677 · stdlib-only PBKDF2)          |
+-------------------------------------------------------------------+
| pgerr (ErrorResponse + NoticeResponse decoder)                    |
+-------------------------------------------------------------------+
| bufferpool (sync.Pool of []byte · optional live cap)              |
+-------------------------------------------------------------------+
```

Everything below `pg2json/` is in `internal/`. The public surface is
small on purpose: `Client`, `Config`, `Observer`, `pool.Pool`,
`ResponseTooLargeError`, and the Mode / Stats types. Anything else is
implementation detail that moves without notice.

## Wire layer

`internal/wire` owns the `net.Conn`, a 32 KiB `bufio.Reader` in front
of it, and a single growable `readBuf` for assembling message bodies.
`ReadMessage` returns the *type byte* and a slice into `readBuf`. The
slice is valid only until the next `ReadMessage` call — that contract
is declared loudly in the doc comment, and every caller respects it.

Two allocation reductions live here:

1. **No per-message allocation.** `readBuf` is a single growing slab
   that keeps the largest-so-far body size. The returned slice aliases
   it.
2. **Buffered reads.** Without `bufio`, each `ReadMessage` was two
   `io.ReadFull` syscalls (header + body). For narrow-column queries
   that meant 2 syscalls × 100k rows = 200k context switches. The
   bufio layer amortises that to ~1 read per 32 KB of traffic. On a
   narrow-int 100k-row query this was the difference between 20 MB/s
   and 100 MB/s.

Writes go through a small per-connection write buffer. Length prefixing
is computed by patching the slice in place after the body is built.

## Protocol parsing

`internal/protocol` is a small package of constants and helpers:

- Frontend / backend message type codes, mirrored from
  `src/include/libpq/protocol.h`.
- Authentication sub-codes (`AuthOK`, `AuthSASL`, …).
- A curated list of type OIDs (`oid.go`) covering every type we
  specialise for — scalars + arrays + interval / time / timetz.
  `ArrayElem(oid)` maps an array OID to its element OID so the binary
  decoder can pick the right per-element function.

## Encoder plan

After a `RowDescription`, we walk the column descriptors and build a
plan:

```go
type Column struct {
    Name      string
    TypeOID   protocol.OID
    Format    int16
    Encoder   types.Encoder     // picked from OID + format
    KeyPrefix []byte             // pre-built `"name":`
}
```

`KeyPrefix` is built once and reused per-row — the row loop is a tight
sequence of `append`s with no string formatting. For columnar output
we additionally pre-build a `{"columns":[...],"rows":[` header.

When we open an extended-protocol `Bind`, we request *binary format*
for every column whose OID has a specialised binary encoder
(`types.HasBinary`). The plan's `Encoder` is then swapped for the
binary variant. Text format remains as a correctness-preserving
fallback for anything we don't specialise.

## JSON writer

`internal/jsonwriter` is a set of `Append*` functions that write into
a caller-supplied `[]byte`. There is no writer struct on purpose:
passing the slice around lets the compiler keep the slice header in
registers and matches the idiom of `strconv.AppendInt`.

String escaping is driven by a 256-entry lookup table (`escapeFlag`)
that records whether each byte must be escaped (0x00–0x1F, `"`, `\`).
The hot loop walks the input, accumulates a "copy span", and only
branches to the slow path when an escapable byte appears. ASCII text
passes through with one `append`.

### Optional SWAR escape path

Under the experimental `pg2json_simd` build tag, the scalar
`AppendStringBody` / `AppendStringBodyBytes` are swapped for a SWAR
(SIMD-Within-A-Register) implementation that checks 8 bytes at once
using `uint64` arithmetic. The key primitive is:

```go
func swarHasEscapable(x uint64) uint64 {
    // Non-zero if any byte of x is < 0x20, == 0x22, or == 0x5C.
    lowCtrl := (x - 0x2020...) & ^x & 0x8080...
    quote   := hasZeroByte(x ^ 0x2222...)
    slash   := hasZeroByte(x ^ 0x5C5C...)
    return lowCtrl | quote | slash
}
```

When an 8-byte chunk is entirely escape-free the loop advances by 8
with no per-byte branch. Measured on Apple M4: **~4× faster on
medium/long ASCII**, ~1.1× on strings with every-4th-byte escapes.
Zero platform-specific code — compiles everywhere Go does. A real
AVX2 / NEON assembly path is a possible follow-up; SWAR is the
pure-Go stepping stone that captures the bulk of the theoretical win
without per-architecture maintenance.

### Fuzz coverage

`FuzzAppendString`, `FuzzAppendStringBytes`, `FuzzAppendKey`,
`FuzzParse` (pgerr), and `FuzzParseRowDescription` (rows) run against
both the scalar and SWAR paths. Millions of inputs per run guarantee
the writers never panic on arbitrary bytes and always produce output
that round-trips through `encoding/json` for valid UTF-8.

## Type encoders

`internal/types` has one file per family:

- `encoder.go` — text-format encoders. Numbers and booleans pass
  through validated; the server's text form is already a valid JSON
  token. Strings are escaped through `jsonwriter`. Anything not
  specifically claimed falls through to quoted-string — always correct
  for the text protocol.
- `binary.go` — binary-format encoders for int2/4/8, float4/8, bool,
  uuid, json, jsonb (strips version byte), bytea, date, timestamp,
  timestamptz. Timestamps decode directly from `int64` µs to an
  ISO-8601 byte sequence without going through `time.Time`.
- `interval.go` — interval/time/timetz. Interval emits ISO-8601
  duration (`P1Y2M3DT4H5M6.789S`). Trailing fractional zeros are
  stripped.
- `array.go` — recursive multi-dim binary array decoder. Reads the
  array header (ndim, flags, elemOID, per-dim sizes), then dispatches
  each element through the scalar binary encoder. Emits nested JSON
  arrays. SQL NULL elements become JSON `null`.
- `numeric.go` — text-format passthrough with a fast validation pass.
  PostgreSQL emits `numeric` as a textual decimal; we drop it into the
  JSON output as a raw number. `NaN` / `Infinity` / `-Infinity` are
  routed to JSON strings (the literals are not valid JSON).
- `bytea.go` — `bytea_output = hex` (the default) is emitted as
  `"\\x..."`. The legacy `escape` form is routed through the generic
  string escaper for correctness.
- `jsonpass.go` — `json` / `jsonb` are passed through verbatim.
  `jsonb` binary has a 1-byte version prefix which we check and strip.
- `uuid.go` — validated length, then hex-formatted directly from the
  16 raw bytes without `encoding/hex` (hand-rolled appender is ~4×
  faster).

The encoder signature is:

```go
type Encoder func(dst []byte, raw []byte) []byte
```

`raw == nil` always means SQL NULL. `DataRow` represents NULL with
length `-1` on the wire; `internal/rows` translates that to a nil
slice. Every encoder handles it explicitly.

## Hot row loop

The per-row loop is in `internal/rows/datarow.go`:

```go
buf = append(buf, '{')
for i, col := range plan.Columns {
    if i > 0 {
        buf = append(buf, ',')
    }
    buf = append(buf, col.KeyPrefix...)
    buf = col.Encoder(buf, cells[i])
}
buf = append(buf, '}')
```

`cells[i]` is a slice into the wire scratch buffer; no copy happened
between the socket and here. The loop allocates zero bytes and is
enforced by a benchmark (`BenchmarkRowEncodeMixed`) that runs in CI
and must report `0 allocs/op` on every change.

## Streaming

`StreamJSON` and `StreamNDJSON` build a `flushingWriter` around the
caller's `io.Writer`:

- Flushes when the in-memory buffer crosses `Config.FlushBytes`
  (default 32 KiB), OR
- When `Config.FlushInterval` (if > 0) has elapsed since the last
  flush. The check is inline in `MaybeFlush`, which is already called
  once per row — no goroutine, no mutex, no timer.

Flushes only happen on row boundaries, so NDJSON consumers that scan
line-by-line never see a split row.

The key invariant: **no bytes leave the writer before `BindComplete`
plus the first `DataRow`**. A query that fails before producing any
row never writes to the downstream writer. Callers can tell they
received a clean failure vs a mid-stream failure by checking
`out.Committed()` (surfaced via `ResponseTooLargeError.Committed`
and the Observer's error event).

## Cancellation

`context.Context` cancellation is handled by a watcher goroutine
installed for the duration of each query (`internal/ctx.go`). On
cancel, the watcher:

1. Opens a side TCP connection to the same server.
2. Sends a `CancelRequest` carrying the Client's backend key data.
   The server aborts the in-flight query with SQLSTATE 57014
   (`query_canceled`).
3. Sets a past deadline on the main socket so any in-flight read
   returns `os.ErrDeadlineExceeded` promptly instead of hanging
   until the cancel arrives.

The watcher is torn down on normal query completion.

## Prepared statement cache and PgBouncer-txn

The extended-query path caches prepared statements by SQL text on the
Client. Each cache miss pays for `Parse + Describe` and adds the plan
to the cache; hits go straight to `Bind + Execute + Sync`.

PgBouncer in transaction mode rotates the physical backend between
transactions. A cached prepared-statement name from a previous
transaction will not exist on the new backend, and `Bind` fails with
SQLSTATE 26000 (`invalid_sql_statement_name`). The cache handler
detects 26000, invalidates the entry, and retries once with a fresh
`Parse` — **but only if no bytes have been flushed downstream yet**.
If anything was already committed, the error surfaces to the caller
(the partial JSON is the caller's problem to discard).

## Citus-specific retry

SQLSTATE 40001 (serialization_failure) and 40P01 (deadlock_detected)
are retried transparently when `Config.RetryOnSerialization = true`
and no bytes have been committed. Up to 3 attempts total, exponential
backoff starting at 10ms. Other class-40 codes (40002, 40003) are not
retried — the first is a data problem and the second leaves server
state ambiguous.

## Defence in depth for timeouts

Three layers, documented explicitly so they are not confused:

1. `postgresql.conf` `statement_timeout` — DBA-owned hard ceiling.
   Final say. Protects against misbehaving clients and client bugs.
2. `Config.DefaultQueryTimeout` — gateway-level sane default applied
   when the caller's ctx has no deadline. Fires a real server-side
   `CancelRequest` via the existing ctx watcher.
3. `ctx.WithTimeout` at the HTTP handler — per-request override.
   Always wins if shorter than (2).

Layers (2) and (3) are convenience. Layer (1) is security.

## Hard caps per response

`Config.MaxResponseBytes` and `Config.MaxResponseRows` trip a
`CancelRequest` plus drain sequence when crossed. The error returned
is `*ResponseTooLargeError` (wraps the `ErrResponseTooLarge`
sentinel). The `Committed` field tells the caller whether the partial
JSON reached the downstream writer — same contract as mid-stream
Citus errors.

The check is one branch per row, outside the per-cell encoder hot
loop. Hot-loop invariants are preserved.

## Pool

`pg2json/pool` is a bounded LIFO pool with:

- `Acquire(ctx)` blocks up to ctx on an empty-and-full pool.
- `MaxConnLifetime` — `Acquire` closes and reopens past the cap
  (bounds server-side state growth on long-lived backends).
- `PingAfterIdle` — `Acquire` pings an idle conn that has been
  sitting longer than the threshold (catches silent TCP kills behind
  NAT / PgBouncer's `server_idle_timeout`).
- `Discard()` — explicit way to retire a poisoned conn without
  returning it to idle.
- Transparent `Acquire` retry (up to 3×) for max-lifetime and ping
  failures.
- `Drain(ctx)` stops accepting new Acquires, closes idle conns
  immediately, waits for in-flight Release; `WaitIdle(ctx)` waits
  without blocking new Acquires. Backed by a `sync.Cond` signalled
  on every Release / Discard.
- `Stats()` snapshot: Open, Idle, InUse, Waiting, Max.

## Observer

A single-struct hook installed with `SetObserver`. Methods:

- `OnQueryStart(sql)` — just before the wire traffic begins.
- `OnQueryEnd(QueryEvent)` — always, with duration, bytes, rows,
  error, SQLSTATE, retry count.
- `OnNotice(*pgerr.Error)` — every `NoticeResponse`. Citus surfaces
  worker diagnostics here; previously dropped.
- `OnQuerySlow(QueryEvent)` — additional dispatch when
  `Config.SlowQueryThreshold` is set and exceeded. Additive to
  `OnQueryEnd`.

Implementations must be safe for concurrent use (a single Observer is
typically shared across all Clients in a pool). The default
no-op observer adds zero overhead. Atomic counters (`Client.Stats()`)
are a lock-free alternative for simple use cases.

## Tradeoffs explicitly chosen

- **No reflection anywhere.** Encoders are picked by OID with a
  `switch`. New types add one function and one case.
- **No `database/sql` adapter.** Mapping pg2json onto a row/cell
  interface would re-introduce per-cell allocations.
- **No ORM features, no struct mapping.** Different tool for
  different job. Use pgx when you need structs.
- **SELECT only.** Mutations are rejected at the API. The design
  invariants (no JSON corruption on error, safe retries) depend on
  not having side effects to worry about.
- **Stdlib only at runtime.** SCRAM uses hand-rolled PBKDF2. The bar
  for adding a dependency is high. `pgx` is a dev dependency,
  test-only.
- **Buffer-pool backpressure is a soft cap, not a blocker.** When the
  live-buffer cap is hit, `Get` returns a fresh non-pooled buffer.
  Blocking would add latency to every burst-scale acquisition and
  could deadlock on interlocked queries; bypass trades a few extra
  allocations during burst for predictable latency.
- **No fancy connect-time protocol negotiation.** We ask for
  protocol 3.0 and accept whatever the server negotiates down to.
  Works against PG 14+ defaults, RDS, Cloud SQL, Supabase, Neon.

## What is NOT here (and why)

- **INSERT / UPDATE / DELETE**: rejected at the API. Different
  workload; different tool.
- **COPY fast-export**: on the roadmap. Would beat the extended-query
  path for bulk exports. Not implemented yet.
- **Numeric binary decoder**: numeric binary is packed-decimal and
  non-trivial. Text form works and is already JSON-number-shaped.
- **LISTEN / NOTIFY / replication**: out of scope.
- **Struct scan, `sql.Scanner`, `driver.Valuer`**: out of scope. The
  whole point is to skip the Go-type round trip.

## Performance budget

A single-row encode on a 6-column mixed shape lives inside ~55 ns and
allocates 0 bytes. At that budget, the ceiling is the wire protocol
itself: on loopback we saturate at ~165 MB/s of JSON output for mixed
5-column rows, which matches a hand-tuned pgx+RawValues implementation
within ±2%. If that ceiling needs to rise, the next lever is COPY
binary — not further optimisation of the per-cell encoders.
