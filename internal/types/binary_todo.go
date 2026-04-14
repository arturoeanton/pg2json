package types

// Binary-format encoders are deferred to Phase 2. The Pick() entrypoint
// would gain a `format int16` argument and dispatch to a parallel set of
// functions:
//
//   PickBinary(oid OID) Encoder
//
// High-value binary types to implement first (in priority order):
//
//   int2/int4/int8 — fixed-width network byte order; trivial.
//   float4/float8  — IEEE 754 in network byte order; strconv.AppendFloat
//                    on the parsed value.
//   bool           — single byte 0/1.
//   uuid           — 16 raw bytes; format as 8-4-4-4-12 hex without
//                    going through encoding/hex (a hand-rolled appender
//                    is ~4x faster).
//   timestamp(tz)  — int64 microseconds since 2000-01-01 (Postgres epoch).
//                    Format directly into ISO 8601 without time.Time.
//   numeric        — variable-length packed decimal; the canonical
//                    reference is src/backend/utils/adt/numeric.c
//                    (numericvar_to_str / set_var_from_num).
//
// json/jsonb in binary format: jsonb has a 1-byte version prefix
// followed by the same bytes as text; json's binary form is identical
// to the text form. So passthrough works either way after stripping the
// version byte for jsonb.
