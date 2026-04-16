# Benchmarks

Hardware: Apple M4 Max, macOS 25.3, Go 1.23+. Server: PostgreSQL 17
in Docker (`docker/docker-compose.yml`) with `shared_buffers=512MB`
and `fsync=off` — a bench box config, not a production one. Loopback
TCP, `sslmode=disable`. 100 000-row tables seeded from
`docker/init.sql`.

Reproduce on your own hardware:

```bash
docker compose -f docker/docker-compose.yml up -d
export PG2JSON_TEST_DSN="postgres://pgopt:pgopt@127.0.0.1:55432/pgopt?sslmode=disable"
go test ./tests -run '^$' -bench BenchmarkFullCompare -benchmem -benchtime=2s -count=3
```

---

## Consolidated comparison — `bench_mixed_5col`, 100 000 rows

Shape: `id int4, name text, score float8, flag bool, meta jsonb`.
Medians of three runs. Outliers (thermal, GC) were discarded when
the distribution was clearly bimodal.

| path | ns/op | allocs/op | bytes/op |
|---|---|---|---|
| **pg2json — JSON output** | | | |
| `StreamNDJSON` | 31.7 ms | **5** | **82 KB** |
| `StreamTOON` | 31.9 ms | 5 | 82 KB |
| `StreamColumnar` | 31.9 ms | 6 | 82 KB |
| `QueryJSON` (buffered) | 31.5 ms | 30 | 53 MB |
| **pg2json — struct scan** | | | |
| `ScanStruct[T]` | 31.8 ms | 200 k | 20 MB |
| `ScanStructBatched[T]` (batch 10 k) | 32.0 ms | 200 k | **3.8 MB** |
| **pg2json — database/sql adapter** | | | |
| `stdlib.Query` | 31.6 ms | 800 k | 12 MB |
| **Reference — pgx paths** | | | |
| `pgx Map+Marshal` (naive NDJSON) | 120 ms | 3.3 M | 155 MB |
| `pgx RawValues + NDJSON hand` | 30.5 ms | 11 | 66 KB |
| `pgx Scan(&f, …)` manual | 30.2 ms | 500 k | 26 MB |
| `pgx CollectRows[ByName]` | 44 ms | 700 k | 70 MB |
| `pgx stdlib.Query` | 29.2 ms | 900 k | 13.5 MB |

---

## Read it this way

### What we win clearly

| our path | comparable pgx path | delta |
|---|---|---|
| `StreamNDJSON` | `pgx Map+Marshal` | **3.8× faster**, 660 000× fewer allocs, 1 890× less memory |
| `ScanStruct[T]` | `pgx CollectRows[ByName]` | **1.38× faster**, 3.5× fewer allocs, 3.5× less memory |
| `ScanStructBatched[T]` | any pgx struct path | same wall time, **~5× less memory** (3.8 MB vs 20–70 MB) |

### Where we tie

| our path | comparable pgx path | delta |
|---|---|---|
| `StreamNDJSON` | `pgx RawValues + hand NDJSON` | same ns/op, 2× fewer allocs (5 vs 11) |
| `ScanStruct[T]` | `pgx Scan(&f, …)` manual | ~5 % slower (within noise), 2.5× fewer allocs |
| `stdlib.Query` | `pgx stdlib.Query` | ~8 % slower, 100 k fewer allocs |

### Where the ~5–8 % gap comes from

The decoder on the client is ~2 ms of a ~32 ms query. Anything in
that band is within run-to-run variance on loopback. The `stdlib`
gap is a layer above — `database/sql` itself boxes every cell value
through `driver.Value` and converts in `Rows.Scan`, which is
inherent to the framework, not to our driver. On LAN / WAN the
wire dominates and these gaps shrink below 1 %.

---

## Allocations under real load

At 10 000 queries per second (a typical gateway), the same
100 000-row shape produces:

| path | allocs/second |
|---|---|
| `pg2json StreamNDJSON` | **50 k/s** |
| `pg2json ScanStructBatched` | 2.0 B/s |
| `pg2json ScanStruct` | 2.0 B/s |
| `pg2json stdlib.Query` | 8.0 B/s |
| `pgx Scan manual` | 5.0 B/s |
| `pgx stdlib.Query` | 9.0 B/s |
| `pgx CollectRows` | 7.0 B/s |
| `pgx Map+Marshal` | 33 B/s |

In production these numbers translate to GC pause frequency and p99
tail latency, not to wall-time on a single bench. The alloc
reduction is the most durable advantage — it shows up identically
on loopback, LAN, and WAN, while the ns/op differences disappear on
real networks.

---

## Micro-benchmarks — hot loop

| benchmark | ns/op | B/op | allocs |
|---|---|---|---|
| `RowEncodeMixed` (6-col row → JSON bytes) | 46 | 0 | **0** |
| `EncodeInt` (text) | 5 | 0 | **0** |
| `EncodeText` (ASCII) | 21 | 0 | **0** |

Zero-alloc per cell is enforced on every change that touches
`internal/rows` or `internal/types` — a bench that runs as part of
the test suite and fails on regression.

---

## Notes on outputs (byte count)

`StreamNDJSON`, `StreamColumnar`, `StreamTOON` all produce the same
information shape for 100 000 `mixed_5col` rows but differ in size:

| mode | bytes / row | bytes total (100 k rows) | relative |
|---|---|---|---|
| NDJSON | 87.6 | 8.4 MB | 100 % |
| Columnar | 53.6 | 5.1 MB | 61 % |
| TOON | 51.6 | 4.9 MB | **59 %** |

Wall time is identical across modes on loopback (the wire cost
dominates). The byte savings translate to wall time on LAN / WAN and
to token count when the consumer is an LLM.

---

## Coverage of `ScanStruct[T]`

All of these ship tested, end-to-end, against real PostgreSQL:

- scalars: `int2/4/8`, `uint32`, `float4/8`, `bool`, `string`,
  `[]byte`, `time.Time`, `json.RawMessage`
- nullable via `*T` (pointer) and via `sql.Null*`
- custom `sql.Scanner` targets (routed through `Scan(any)`)
- 1-D and 2-D arrays (`[]int32`, `[][]int32`, etc.)
- embedded structs (Go-promotion semantics, outer shadows inner)
- range types (`int4range`, `tstzrange`, …) via `pg2json.RangeBytes`

Not supported (by design, use pgx or an `sql.Scanner` custom type):
composite types (`ROW(…)`), 3-D+ arrays, `pgtype.*` wrappers.

---

## Honest limitations

- Numbers measured on Apple M4 loopback. The ordering should hold on
  comparable hardware; absolute values move.
- No 8-hour / 500 k-user soak. Design targets it; validation
  pending.
- `database/sql` adapter is 5–8 % slower than the native API due to
  the framework's own interface boxing. Use native when the read
  path is in a hot spot.
- COPY binary export path is on the roadmap, not implemented — the
  only meaningful lever still on the table for bulk-export
  workloads beyond the ~165 MB/s ceiling we see on mixed shapes
  today.
