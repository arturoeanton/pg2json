// Package types maps PostgreSQL type OIDs to JSON-emitting encoder
// functions. Encoders take the raw text-format bytes from a DataRow cell
// and append the JSON form to the destination buffer.
//
// The contract:
//   - raw == nil means SQL NULL; the encoder writes the JSON literal null.
//   - the returned slice is dst with the cell appended. No allocations
//     for the common cases (numbers, bools, json passthrough).
//   - encoders never retain raw beyond their own call.
package types

import (
	"github.com/arturoeanton/pg2json/internal/jsonwriter"
	"github.com/arturoeanton/pg2json/internal/protocol"
)

type Encoder func(dst, raw []byte) []byte

// Pick returns the text-format encoder for the given OID. Anything we do
// not specifically know about is rendered as a JSON string — that is
// always correct for the text protocol because the server has already
// produced a textual rendering.
func Pick(oid protocol.OID) Encoder {
	switch oid {
	case protocol.OIDBool:
		return EncodeBool
	case protocol.OIDInt2, protocol.OIDInt4, protocol.OIDInt8, protocol.OIDOID:
		return EncodeInt
	case protocol.OIDFloat4, protocol.OIDFloat8:
		return EncodeFloat
	case protocol.OIDNumeric:
		return EncodeNumeric
	case protocol.OIDJSON, protocol.OIDJSONB:
		return EncodeJSONPassthrough
	case protocol.OIDBytea:
		return EncodeBytea
	case protocol.OIDUUID:
		return EncodeUUID
	case protocol.OIDDate, protocol.OIDTime, protocol.OIDTimeTZ,
		protocol.OIDTimestamp, protocol.OIDTimestampTZ, protocol.OIDInterval:
		return EncodeQuotedASCII
	case protocol.OIDText, protocol.OIDVarchar, protocol.OIDBPChar, protocol.OIDName:
		return EncodeString
	default:
		return EncodeString
	}
}

// EncodeString quotes and escapes the raw bytes as a JSON string.
func EncodeString(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	return jsonwriter.AppendStringBytes(dst, raw)
}

// EncodeQuotedASCII is a fast path for values we know are pure ASCII with
// no characters that need escaping (timestamps, dates, intervals as the
// server formats them). We still validate cheaply.
func EncodeQuotedASCII(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	for _, c := range raw {
		if c < 0x20 || c == '"' || c == '\\' {
			// Defensive: fall back to full escape. This should never
			// trigger for well-formed Postgres timestamp output but
			// guarantees correctness if e.g. a custom datestyle ever
			// includes weird bytes.
			return jsonwriter.AppendStringBytes(dst, raw)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, raw...)
	return append(dst, '"')
}
