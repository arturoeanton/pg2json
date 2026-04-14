# Architecture

## Layers

```
+--------------------------------------------------------------+
|  pg2json (public API: Client, QueryJSON, StreamJSON, ...)    |
+--------------------------------------------------------------+
|  rows  (encoder plan, hot row loop)                          |
+--------------------------------------------------------------+
|  jsonwriter   |   types (per-OID encoder funcs)              |
+--------------------------------------------------------------+
|  protocol (message codes, OIDs, parsing helpers)             |
+--------------------------------------------------------------+
|  wire (framed reader/writer over net.Conn, scratch buffer)   |
+--------------------------------------------------------------+
|  auth (cleartext, md5; SCRAM stub)                           |
+--------------------------------------------------------------+
```

Everything below `pg2json/` is in `internal/`. The public surface is small on
purpose.

## Wire layer

`internal/wire` owns the `net.Conn` and a single growable scratch buffer per
connection. `ReadMessage` returns the *type byte* and a slice into the
scratch buffer. The slice is valid only until the next `ReadMessage` — that
contract is made very loud in the doc comment, and the rest of the codebase
respects it: encoders that need to keep the bytes (almost none, because they
write straight into the JSON output) copy explicitly.

This is the key allocation reduction over a more naive driver: we never
allocate a `[]byte` per `DataRow`.

Writes go through a small per-connection `[]byte` that is reused message by
message. Length prefixing is computed by patching the slice in place.

## Protocol parsing

`internal/protocol` is a small package of constants and helpers:

- Frontend / backend message codes, mirrored from
  `src/include/libpq/protocol.h` in the Postgres tree.
- `Auth*` constants from the same header.
- A few `binary.BigEndian` helpers that are inlined at call sites.
- A curated list of type OIDs (`internal/protocol/oid.go`) — only the ones
  we actually treat specially. Everything else falls through to the string
  encoder, which is correct for the text protocol because the server already
  produces a textual representation we can quote.

## Encoder plan

After `RowDescription`, we walk the column descriptors and pick one
`types.Encoder` per column based on the type OID. The plan is an
`[]Encoder` with parallel `[][]byte` for the column-name JSON keys (already
pre-quoted with the trailing `:` so the row loop just appends). For NDJSON
and array-of-objects we also pre-build a key-prefix per column to keep the
loop branch-free.

```go
type Encoder func(dst []byte, raw []byte) []byte
```

`raw == nil` always means SQL NULL — `DataRow` signals NULL with length `-1`
in the wire format and we translate that to a nil slice. Each encoder is
responsible for handling it (most just delegate to `appendNull`).

The plan is built once per result-set shape and cached on the `Client`
keyed by the SQL string + parameter OIDs (Phase 2 — currently the cache key
is just the SQL string; see `pg2json/conn.go`).

## JSON writer

`internal/jsonwriter` is a small package of `Append*` functions that write
into a caller-supplied `[]byte`. There is no writer struct — passing the
buffer back and forth as a `[]byte` lets the compiler keep the slice header
in registers and matches the idiom of `strconv.AppendInt`, which is the
fastest path in the standard library.

String escaping uses a 256-entry lookup table that records, for each byte,
whether it must be escaped. The hot loop walks the input, accumulates a
"copy span", and only branches into the slow path when an escapable byte
appears. ASCII text passes through with one `append`.

## Type encoders

`internal/types` has one file per family of types. The interesting ones:

- `numeric.go`: integers and floats. We **do not** parse and reformat —
  the server already gave us a valid JSON-compatible decimal string for the
  text protocol (`1234`, `-3.14e10`). We do a fast validation pass to make
  sure it is safe to drop into the JSON output as a raw number, and fall
  back to a quoted string for `NaN`, `Infinity`, `-Infinity` (which Postgres
  emits for `numeric` and floats but JSON does not allow).
- `jsonpass.go`: `json` and `jsonb` are passed through verbatim. `jsonb`
  in text protocol is already canonical JSON.
- `bytea.go`: `bytea_output` defaults to `hex`, e.g. `\x48656c6c6f`. We emit
  `"\\x48656c6c6f"` (escaped backslash). The legacy `escape` form is also
  accepted but routed through the generic string escaper.
- `times.go`: dates and timestamps are quoted. We do **not** reformat the
  timestamp — Postgres' textual ISO 8601 form is already valid for most
  consumers, and reformatting would require parsing into `time.Time` (an
  allocation we are explicitly avoiding). A `TODO` marks the spot if you
  ever need a strict RFC 3339 normaliser.
- `uuid.go`: validated length only, then quoted. UUIDs are pure ASCII.

## Hot row loop

The loop in `rows/loop.go` is intentionally short:

```go
buf = append(buf, '{')
for i, enc := range plan.Encoders {
    if i > 0 { buf = append(buf, ',') }
    buf = append(buf, plan.KeyPrefix[i]...)   // pre-built `"name":`
    buf = enc(buf, columns[i])                 // append cell JSON
}
buf = append(buf, '}')
```

`columns[i]` is a `[]byte` slice into the scratch read buffer; no copy was
made between the socket and here.

## Streaming

`StreamJSON` and `StreamNDJSON` flush every N bytes (default 32 KiB; tunable
via `Config.FlushBytes`) so the downstream consumer (HTTP `ResponseWriter`,
TCP socket, etc.) starts seeing data while the query is still being
streamed by the server. The buffer is taken from `internal/bufferpool` and
returned on completion.

For NDJSON we flush on row boundaries when the buffer crosses the threshold;
the wire reader is not blocked by the flush because flushes happen between
`ReadMessage` calls.

## Cancellation

A `context.Context` cancel triggers `conn.SetDeadline(time.Now())`, which
unblocks any in-flight read with `os.ErrDeadlineExceeded`. We then send a
`CancelRequest` on a side connection (Phase 2 — currently we just close the
connection; the TODO is in `pg2json/conn.go`).

## Tradeoffs explicitly chosen

- **Text format only (Phase 1).** Binary format is faster for some types
  (numeric, timestamp, UUID), but the server's text format is already
  *trivially* convertible to JSON for the common cases (numbers, bools,
  json/jsonb passthrough) which is what the gateway use case sees most. We
  picked the simpler/correct path first; binary is wired up as a switch in
  the Encoder selector.
- **No reflection.** Encoders are explicit functions selected by OID. New
  types are added by writing one function and one switch entry.
- **Single connection per Client.** Pooling is a separate concern; the
  caller can wrap `Client` in any pool they like. Mixing pooling into a
  performance-critical client is a recipe for accidental sharing bugs.
- **No `interface{}` per cell.** Cells are `[]byte`. The encoder is the
  only thing that knows the type.
