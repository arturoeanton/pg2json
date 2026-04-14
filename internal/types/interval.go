package types

import (
	"encoding/binary"
	"strconv"

	"github.com/arturoeanton/pg2json/internal/jsonwriter"
)

// Interval binary format (interval_send in timestamp.c):
//
//	int64 microseconds   (time component)
//	int32 days
//	int32 months
//
// We emit ISO 8601 duration, e.g. "P1Y2M3DT4H5M6.789012S". Negative
// components are rendered with a leading '-' on the whole duration iff
// all non-zero parts are negative; otherwise we honour per-component
// signs (ISO 8601 allows this via '-' prefixes inside the duration).
// Empty interval → "PT0S".
func encodeIntervalBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 16 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	micros := int64(binary.BigEndian.Uint64(raw[0:8]))
	days := int32(binary.BigEndian.Uint32(raw[8:12]))
	months := int32(binary.BigEndian.Uint32(raw[12:16]))

	years := months / 12
	months -= years * 12

	if years == 0 && months == 0 && days == 0 && micros == 0 {
		return append(dst, '"', 'P', 'T', '0', 'S', '"')
	}
	dst = append(dst, '"', 'P')
	if years != 0 {
		dst = strconv.AppendInt(dst, int64(years), 10)
		dst = append(dst, 'Y')
	}
	if months != 0 {
		dst = strconv.AppendInt(dst, int64(months), 10)
		dst = append(dst, 'M')
	}
	if days != 0 {
		dst = strconv.AppendInt(dst, int64(days), 10)
		dst = append(dst, 'D')
	}
	if micros != 0 {
		dst = append(dst, 'T')
		hours := micros / 3_600_000_000
		micros -= hours * 3_600_000_000
		mins := micros / 60_000_000
		micros -= mins * 60_000_000
		secs := micros / 1_000_000
		frac := micros - secs*1_000_000
		if hours != 0 {
			dst = strconv.AppendInt(dst, hours, 10)
			dst = append(dst, 'H')
		}
		if mins != 0 {
			dst = strconv.AppendInt(dst, mins, 10)
			dst = append(dst, 'M')
		}
		if secs != 0 || frac != 0 {
			dst = strconv.AppendInt(dst, secs, 10)
			if frac != 0 {
				dst = append(dst, '.')
				dst = appendFrac6(dst, frac)
			}
			dst = append(dst, 'S')
		}
	}
	return append(dst, '"')
}

// appendFrac6 writes a 6-digit fractional part, stripping trailing zeros
// for a cleaner JSON representation ("0.1" rather than "0.100000").
func appendFrac6(dst []byte, frac int64) []byte {
	if frac < 0 {
		frac = -frac
	}
	buf := [6]byte{
		byte('0' + frac/100000%10),
		byte('0' + frac/10000%10),
		byte('0' + frac/1000%10),
		byte('0' + frac/100%10),
		byte('0' + frac/10%10),
		byte('0' + frac%10),
	}
	end := 6
	for end > 1 && buf[end-1] == '0' {
		end--
	}
	return append(dst, buf[:end]...)
}

// Time binary format: int64 microseconds since midnight.
func encodeTimeBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 8 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	usec := int64(binary.BigEndian.Uint64(raw))
	dst = append(dst, '"')
	dst = appendTimeOfDay(dst, usec)
	return append(dst, '"')
}

// TimeTZ binary format: int64 microseconds since midnight + int32 tz
// offset in seconds EAST of UTC (ISO sign — PG stores it negated, see
// timetz_send). We emit ISO "HH:MM:SS[.ffffff]±HH:MM".
func encodeTimeTZBinary(dst, raw []byte) []byte {
	if raw == nil {
		return jsonwriter.AppendNull(dst)
	}
	if len(raw) != 12 {
		return jsonwriter.AppendStringBytes(dst, raw)
	}
	usec := int64(binary.BigEndian.Uint64(raw[0:8]))
	zone := int32(binary.BigEndian.Uint32(raw[8:12]))
	dst = append(dst, '"')
	dst = appendTimeOfDay(dst, usec)
	// PG sends zone with the *reversed* sign convention (seconds west of
	// UTC as a positive number). Invert to ISO (east-of-UTC positive).
	zone = -zone
	if zone >= 0 {
		dst = append(dst, '+')
	} else {
		dst = append(dst, '-')
		zone = -zone
	}
	h := int(zone / 3600)
	m := int((zone % 3600) / 60)
	dst = append2(dst, h)
	dst = append(dst, ':')
	dst = append2(dst, m)
	return append(dst, '"')
}

func appendTimeOfDay(dst []byte, usec int64) []byte {
	if usec < 0 {
		usec = 0
	}
	hours := usec / 3_600_000_000
	usec -= hours * 3_600_000_000
	mins := usec / 60_000_000
	usec -= mins * 60_000_000
	secs := usec / 1_000_000
	frac := usec - secs*1_000_000
	dst = append2(dst, int(hours))
	dst = append(dst, ':')
	dst = append2(dst, int(mins))
	dst = append(dst, ':')
	dst = append2(dst, int(secs))
	if frac != 0 {
		dst = append(dst, '.')
		dst = appendFrac6(dst, frac)
	}
	return dst
}
