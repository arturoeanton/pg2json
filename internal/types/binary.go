package types

import (
	"encoding/binary"
	"math"
	"strconv"

	"github.com/arturoeanton/pg2json/internal/jsonwriter"
	"github.com/arturoeanton/pg2json/internal/protocol"
)

// PickBinary returns a binary-format encoder for the given OID, or nil if
// we have no specialised binary path for it (callers should fall back to
// requesting text format for that column).
func PickBinary(oid protocol.OID) Encoder {
	switch oid {
	case protocol.OIDBool:
		return encodeBoolBinary
	case protocol.OIDInt2:
		return encodeInt2Binary
	case protocol.OIDInt4, protocol.OIDOID:
		return encodeInt4Binary
	case protocol.OIDInt8:
		return encodeInt8Binary
	case protocol.OIDFloat4:
		return encodeFloat4Binary
	case protocol.OIDFloat8:
		return encodeFloat8Binary
	case protocol.OIDUUID:
		return encodeUUIDBinary
	case protocol.OIDTimestamp:
		return encodeTimestampBinary
	case protocol.OIDTimestampTZ:
		return encodeTimestampTZBinary
	case protocol.OIDDate:
		return encodeDateBinary
	case protocol.OIDJSONB:
		return encodeJSONBBinary
	case protocol.OIDJSON:
		return EncodeJSONPassthrough // identical bytes in binary
	case protocol.OIDInterval:
		return encodeIntervalBinary
	case protocol.OIDTime:
		return encodeTimeBinary
	case protocol.OIDTimeTZ:
		return encodeTimeTZBinary
	case protocol.OIDText, protocol.OIDVarchar, protocol.OIDBPChar,
		protocol.OIDName, protocol.OIDBytea:
		// For variable-length text-shaped types the binary format gives
		// us no win and the JSON escaper is happy with raw bytes.
		// Bytea binary is raw bytes — we render as hex below.
		if oid == protocol.OIDBytea {
			return encodeByteaBinary
		}
		return EncodeString
	}
	if elem := protocol.ArrayElem(oid); elem != 0 {
		return makeArrayBinaryEncoder(elem)
	}
	return nil
}

// HasBinary reports whether PickBinary returns a non-nil specialised
// encoder for oid (i.e. asking for binary is worth it).
func HasBinary(oid protocol.OID) bool { return PickBinary(oid) != nil }

func encodeBoolBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) == 1 && raw[0] != 0 {
		return jsonwriter.AppendBool(dst, true)
	}
	return jsonwriter.AppendBool(dst, false)
}

func encodeInt2Binary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 2 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	v := int16(binary.BigEndian.Uint16(raw))
	return strconv.AppendInt(dst, int64(v), 10)
}

func encodeInt4Binary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 4 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	v := int32(binary.BigEndian.Uint32(raw))
	return strconv.AppendInt(dst, int64(v), 10)
}

func encodeInt8Binary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 8 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	v := int64(binary.BigEndian.Uint64(raw))
	return strconv.AppendInt(dst, v, 10)
}

func encodeFloat4Binary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 4 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	bits := binary.BigEndian.Uint32(raw)
	v := math.Float32frombits(bits)
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return appendNonFiniteFloat(dst, float64(v))
	}
	return strconv.AppendFloat(dst, float64(v), 'g', -1, 32)
}

func encodeFloat8Binary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 8 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	bits := binary.BigEndian.Uint64(raw)
	v := math.Float64frombits(bits)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return appendNonFiniteFloat(dst, v)
	}
	return strconv.AppendFloat(dst, v, 'g', -1, 64)
}

func appendNonFiniteFloat(dst []byte, v float64) []byte {
	switch {
	case math.IsNaN(v):
		return append(dst, '"', 'N', 'a', 'N', '"')
	case math.IsInf(v, 1):
		return append(dst, '"', 'I', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	default:
		return append(dst, '"', '-', 'I', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
}

const hexLower = "0123456789abcdef"

func encodeUUIDBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 16 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	// "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
	dst = append(dst, '"')
	dst = appendHexBytes(dst, raw[0:4])
	dst = append(dst, '-')
	dst = appendHexBytes(dst, raw[4:6])
	dst = append(dst, '-')
	dst = appendHexBytes(dst, raw[6:8])
	dst = append(dst, '-')
	dst = appendHexBytes(dst, raw[8:10])
	dst = append(dst, '-')
	dst = appendHexBytes(dst, raw[10:16])
	dst = append(dst, '"')
	return dst
}

func appendHexBytes(dst, src []byte) []byte {
	for _, b := range src {
		dst = append(dst, hexLower[b>>4], hexLower[b&0xF])
	}
	return dst
}

func encodeByteaBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	dst = append(dst, '"', '\\', '\\', 'x')
	dst = appendHexBytes(dst, raw)
	dst = append(dst, '"')
	return dst
}

// jsonb binary format: 1-byte version (currently 1) followed by the same
// canonical UTF-8 bytes you get in text. Reference:
// src/backend/utils/adt/jsonb.c (jsonb_send / jsonb_recv).
func encodeJSONBBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) < 1 {
		return append(dst, '"', '"')
	}
	if raw[0] != 1 {
		// Future jsonb version we don't understand; emit as escaped string.
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	return append(dst, raw[1:]...)
}

// --- Date / Timestamp ---
//
// PostgreSQL stores timestamps as int64 microseconds from the epoch
// 2000-01-01 00:00:00 UTC ("PostgresEpochJDate"). Date is int32 days from
// the same epoch. We format directly into ISO 8601 without going through
// time.Time. Reference: src/backend/utils/adt/timestamp.c (timestamp_send).

const (
	pgEpochUnix int64 = 946684800 // seconds from Unix epoch to PG epoch
	usecPerDay  int64 = 86400 * 1_000_000
)

// special timestamp sentinels: math.MaxInt64 => "infinity", math.MinInt64 => "-infinity".

func encodeTimestampBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 8 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	usec := int64(binary.BigEndian.Uint64(raw))
	if usec == math.MaxInt64 {
		return append(dst, '"', 'i', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
	if usec == math.MinInt64 {
		return append(dst, '"', '-', 'i', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
	dst = append(dst, '"')
	dst = appendISOTimestamp(dst, usec, "")
	return append(dst, '"')
}

func encodeTimestampTZBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 8 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	usec := int64(binary.BigEndian.Uint64(raw))
	if usec == math.MaxInt64 {
		return append(dst, '"', 'i', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
	if usec == math.MinInt64 {
		return append(dst, '"', '-', 'i', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
	dst = append(dst, '"')
	dst = appendISOTimestamp(dst, usec, "Z")
	return append(dst, '"')
}

func encodeDateBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 4 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	days := int32(binary.BigEndian.Uint32(raw))
	if days == math.MaxInt32 {
		return append(dst, '"', 'i', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
	if days == math.MinInt32 {
		return append(dst, '"', '-', 'i', 'n', 'f', 'i', 'n', 'i', 't', 'y', '"')
	}
	usec := int64(days) * usecPerDay
	dst = append(dst, '"')
	dst = appendISODateOnly(dst, usec)
	return append(dst, '"')
}

// appendISOTimestamp writes "YYYY-MM-DDTHH:MM:SS[.ffffff]<tzSuffix>".
// If tzSuffix is "" the result is a naked timestamp.
func appendISOTimestamp(dst []byte, usec int64, tzSuffix string) []byte {
	sec := pgEpochUnix + usec/1_000_000
	frac := usec % 1_000_000
	if frac < 0 {
		// round towards -infinity for the seconds split
		sec--
		frac += 1_000_000
	}
	y, mo, d, hh, mm, ss := splitTime(sec)
	dst = appendInt4(dst, y)
	dst = append(dst, '-')
	dst = append2(dst, mo)
	dst = append(dst, '-')
	dst = append2(dst, d)
	dst = append(dst, 'T')
	dst = append2(dst, hh)
	dst = append(dst, ':')
	dst = append2(dst, mm)
	dst = append(dst, ':')
	dst = append2(dst, ss)
	if frac != 0 {
		dst = append(dst, '.')
		dst = append6(dst, int(frac))
	}
	dst = append(dst, tzSuffix...)
	return dst
}

func appendISODateOnly(dst []byte, usec int64) []byte {
	sec := pgEpochUnix + usec/1_000_000
	y, mo, d, _, _, _ := splitTime(sec)
	dst = appendInt4(dst, y)
	dst = append(dst, '-')
	dst = append2(dst, mo)
	dst = append(dst, '-')
	dst = append2(dst, d)
	return dst
}

// splitTime is a Unix-seconds → (year, month, day, hour, min, sec) decode
// using the proleptic Gregorian calendar. Adapted from the algorithm in
// time.absDate / Howard Hinnant's date utilities — direct enough not to
// touch time.Time.
func splitTime(sec int64) (year int, month, day, hour, min, second int) {
	const secondsPerDay = 86400
	days := sec / secondsPerDay
	rem := sec - days*secondsPerDay
	if rem < 0 {
		rem += secondsPerDay
		days--
	}
	hour = int(rem / 3600)
	rem -= int64(hour) * 3600
	min = int(rem / 60)
	second = int(rem - int64(min)*60)

	// Days since 1970-01-01; convert to civil date.
	z := days + 719468
	era := z
	if z < 0 {
		era = z - 146096
	}
	era /= 146097
	doe := uint64(z - era*146097)            // [0, 146096]
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := int64(yoe) + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100) // [0, 365]
	mp := (5*doy + 2) / 153                  // [0, 11]
	d := doy - (153*mp+2)/5 + 1              // [1, 31]
	var m uint64
	if mp < 10 {
		m = mp + 3
	} else {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	year = int(y)
	month = int(m)
	day = int(d)
	return
}

func appendInt4(dst []byte, v int) []byte {
	if v < 0 {
		dst = append(dst, '-')
		v = -v
	}
	if v >= 10000 {
		return strconv.AppendInt(dst, int64(v), 10)
	}
	return append(dst,
		byte('0'+v/1000%10),
		byte('0'+v/100%10),
		byte('0'+v/10%10),
		byte('0'+v%10))
}

func append2(dst []byte, v int) []byte {
	return append(dst, byte('0'+v/10), byte('0'+v%10))
}

func append6(dst []byte, v int) []byte {
	return append(dst,
		byte('0'+v/100000%10),
		byte('0'+v/10000%10),
		byte('0'+v/1000%10),
		byte('0'+v/100%10),
		byte('0'+v/10%10),
		byte('0'+v%10))
}
