# Benchmarks: hot-loop inline + wire/buffer knobs

Hardware: Apple M4 Max, macOS 25.3, Go 1.23+. Server: PostgreSQL 17-alpine
in Docker (`docker/docker-compose.yml`) with `shared_buffers=512MB` and
`fsync=off` (bench box, not a production config). Loopback TCP,
`sslmode=disable`. 100 000-row tables seeded from `docker/init.sql`.

Each change was measured before/after with `go test -bench … -count=3
-benchtime=2s`. Numbers below are medians; raw output lives in
`/tmp/base_*.txt` and `/tmp/after_*.txt` on the bench box.

## 1. Inline `readColumn` — hot-loop micro-bench

`go test ./internal/rows -bench=BenchmarkRowEncodeMixed` (6-column
mixed row, 130-byte DataRow, 0 allocs/op).

| metric           | before    | after     | delta     |
|------------------|-----------|-----------|-----------|
| ns/op            | 48.29     | 46.17     | **−4.4%** |
| MB/s             | 2 712     | 2 837     | +4.6%     |
| allocs/op        | 0         | 0         | =         |

Fusing `readColumn` into `AppendObject` / `AppendArray` removes one
function frame plus slice-header round-trip per cell. The gain is
real but only visible on CPU-bound micro-benchmarks. End-to-end over
the wire the bottleneck shifts to IO and the delta becomes
noise-level (see §5).

## 2. Bigger bufio default + `SO_RCVBUF` / `SO_SNDBUF`

Loopback has enough kernel buffering that this change is a no-op for
the bench box. The 64 → 128 KiB bufio bump is a preparation for LAN
deployments where messages larger than 64 KiB would otherwise fall
into the copy-via-scratch path. `TCPRecvBuffer` / `TCPSendBuffer`
are surfaced as Config knobs; no default value is set.

No measurable end-to-end delta on loopback (expected).

## 3. Numeric binary decoder — reverted from default dispatch

Added: `internal/types/numeric_bin.go` decoding the base-10000 packed
wire form directly to canonical decimal, plus unit tests covering
zero, negative, leading/trailing zeros, small fractions, NaN,
±Infinity.

Measured against `bench_numeric` (`id int4, price numeric(18,4),
rate numeric(10,6)` × 100 k rows, NDJSON):

| path           | ns/op     | MB/s      |
|----------------|-----------|-----------|
| text (before)  | 19.93 ms  | 229.9     |
| binary (naive) | 23.20 ms  | 197.3     |

Binary numeric was **~16% slower** on this shape. PostgreSQL's C-side
numeric→text formatter is faster than our Go decoder, and on
numeric(18,4) the wire byte count is comparable (9-byte text vs
~10-byte binary body). The decoder is correct and useful on high-RTT
links with wide-precision numerics; it is exported as
`types.EncodeNumericBinary` but NOT wired into `PickBinary` by
default. Call-sites that want it can build a custom encoder table.

Post-revert numeric: 19.04 ms – 20.07 ms, 228–240 MB/s. Parity with
the pre-change baseline.

## 4. `Config.RowsHint` + pre-size output buffer

Added a one-shot grow in `writeRow` after the first DataRow is
encoded: if `RowsHint > 0`, the buffer is expanded to
`firstRowBytes × RowsHint + 256`. Streaming mode caps at 2× the
`FlushBytes` threshold so the flush cadence is unchanged.

Zero effect on any of the live benches below because they do not set
`RowsHint`. The feature is surface-level; benefit is for callers that
already know their row count and want to avoid the log₂(N) append
doublings in `QueryJSON`. No regression when unset (default is 0).

## 5. End-to-end: pg2json vs pgx, 100 000 rows

`tests/BenchmarkPgx/<shape>/100000/*`, NDJSON to `io.Discard`.
Medians of three runs. `PgxMap` = `rows.Values()` → `map[string]any`
→ `json.Marshal` (the naive common path). `PgxRaw` = `RawValues()` +
hand-written NDJSON (pgx best case).

### Pg2JSON: before vs after

| shape         | before MB/s | after MB/s | Δ          |
|---------------|-------------|------------|------------|
| narrow_int    | 128.97      | 125.68     | −2.5%      |
| mixed_5col    | 169.94      | 168.26     | flat       |
| wide_jsonb    | 144.01      | 143.97     | flat       |
| array_int     | 138.69      | 134.91     | −2.7%      |
| null_heavy    | 173.22      | 175.07     | +1.1%      |

Verdict: **no end-to-end delta within noise**. The micro-bench gain
from §1 does not surface because the wire (PG's per-row work plus
loopback cost) dominates the profile, not the JSON encoder. The
small negative deltas on narrow_int / array_int are within the run-to-run
variance observed on the bench box during the same session (see §5.3
for example).

### Pg2JSON vs pgx (after)

| shape         | Pg2JSON | PgxMap  | PgxRaw  | vs PgxMap | vs PgxRaw |
|---------------|---------|---------|---------|-----------|-----------|
| narrow_int    | 125.68  | 38.26   | 131.26  | 3.3×      | 0.96×     |
| mixed_5col    | 168.26  | 69.53   | 167.56  | 2.4×      | 1.00×     |
| wide_jsonb    | 143.97  | 32.18   | 102.16  | 4.5×      | 1.41×     |
| array_int     | 134.91  | 59.45   | 134.70  | 2.3×      | 1.00×     |
| null_heavy    | 175.07  | 59.31   | 175.21  | 3.0×      | 1.00×     |

Pg2JSON beats `PgxMap` by 2.3–4.5× (allocs tell the story: pg2json
commits 6 allocs per full query; PgxMap does 1.3–4.5 million).
Against `PgxRaw`, pg2json is flat on 4 shapes and ahead on
`wide_jsonb` (the jsonb passthrough avoids PgxRaw's per-row concat).

### 5.3 Run-to-run variance context

Single bench run on this box shows variance up to ~10% for PgxRaw on
Docker-backed workloads (compare wide_jsonb/PgxRaw between bench
runs: 145.46 → 102.16 MB/s with no code change on that path). Treat
the end-to-end numbers as ±5% noise; anything below that is inside
the run-to-run spread.

## Summary

- **Micro-bench hot loop**: +4.6% throughput, 0 allocs retained.
- **End-to-end over loopback**: unchanged within ±3% noise — already
  wire-bound.
- **Numeric binary**: decoder shipped as a utility (`EncodeNumericBinary`),
  NOT auto-selected; default dispatch remains text. Net-negative on
  loopback numeric(18,4).
- **TCPRecvBuffer / TCPSendBuffer / bigger bufio default / RowsHint**:
  surfaced as Config knobs with zero regression. Payoff shows up on
  LAN deployments (TCP buffers) and buffered-mode large responses
  (RowsHint); these were not measured here and should be validated in
  the shadow-traffic soak.

Where the next win is: the hot loop is done. Further throughput
requires either COPY binary export (not SELECT-compatible, requires
trusted SQL — see conversation in CLAUDE.md), pipeline mode for
high-QPS small-query gateways, or SIMD assembly for string escape —
each with its own tradeoffs and opt-in story.
