# Roadmap

## Done so far

- TLS negotiation (`SSLRequest`), modes disable/prefer/require/verify-ca/verify-full.
- SCRAM-SHA-256 (RFC 7677, validated with the official test vector).
- Connection pool with bounded concurrency, idle reaper, `Discard` for poisoned conns.
- Prepared statement cache (per-connection, bounded, with server-side `Close` batching).
- Binary result format for: bool, int2/4/8, float4/8, uuid, json, jsonb, bytea, date, timestamp, timestamptz.
- Real `CancelRequest` on a side connection driven by `context.Cancel`.
- Comparison benchmark vs `scan + map + json.Marshal` (3.6× faster, 37,000× fewer allocs).
- End-to-end harness in `cmd/pg2json_bench`.

## Phase 1 — Original deliverable (now superseded by the above)

- TCP connect, StartupMessage, parameter status, BackendKeyData, ReadyForQuery.
- Auth: trust, cleartext, MD5.
- Simple query (`Q`) path.
- Extended query (`Parse`/`Bind`/`Describe`/`Execute`/`Sync`) for parameterised
  selects, text format.
- RowDescription / DataRow / CommandComplete / ErrorResponse parsing.
- Per-shape compiled column encoder plan.
- JSON output: array-of-objects, NDJSON, columnar.
- JSON writer with correct escaping.
- Type fast paths: bool, int2/4/8, float4/8, numeric, text/varchar/bpchar,
  name, uuid, json, jsonb, bytea, date, timestamp, timestamptz.
- Buffer pool.
- Reject non-SELECT statements at the API.
- Tests for JSON writer, type encoders, framing.
- Benchmarks for the JSON writer and the row encoder hot loop, plus an
  end-to-end harness that talks to a real PostgreSQL when `PG2JSON_TEST_DSN`
  is set.

## Phase 2 — Next

- Numeric binary decoder (current path validates text; binary needs a
  packed-decimal walker — reference `numericvar_to_str` in
  `src/backend/utils/adt/numeric.c`).
- Array binary decoder (recursive, per-element encoder).
- Time / timetz / interval binary decoders.
- COPY-binary fast export path: `COPY (SELECT …) TO STDOUT WITH (FORMAT
  binary)` skips the per-row protocol overhead entirely. Useful for bulk
  exports.
- Pluggable metrics hook (rows out, bytes out, query duration).
- Connect timeouts honour `context.Context` for the full handshake, not
  just the dial.

## Phase 3 — Later

- More types: arrays (per-element encoder, recursive), inet/cidr, interval,
  enum text passthrough, range types as JSON objects.
- A simple connection pool in a sibling package (kept out of the hot path).
- Metrics hooks (rows out, bytes out, query duration) — pluggable, no
  default dependency.
- Optional zstd or gzip writer wrapper for the streaming API.
- Fuzz tests for the JSON writer and the protocol parser.
- A `pgcopy2json` mode that uses `COPY (SELECT …) TO STDOUT WITH (FORMAT
  binary)` for the absolute fastest bulk export path. (Postgres' `COPY`
  text format is comma-and-newline noise that we would just have to undo;
  binary `COPY` is what we want here.)

## Explicitly not planned

- INSERT/UPDATE/DELETE.
- LISTEN/NOTIFY.
- Replication protocol.
- ORM features.
- `database/sql` adapter.
- A "scan into struct" API.
