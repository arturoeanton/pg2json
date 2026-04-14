package pg2json

import (
	"fmt"
	"strconv"
)

// encodeArg renders a parameter as the text wire form. Returns
// (value, isNull, error). We accept the common scalar Go types and let the
// server do the type inference; this keeps the API small and avoids
// mismatches between Go and Postgres types.
func encodeArg(v any) (string, bool, error) {
	switch x := v.(type) {
	case nil:
		return "", true, nil
	case string:
		return x, false, nil
	case []byte:
		// Render as bytea hex literal: "\\x..."
		const hex = "0123456789abcdef"
		buf := make([]byte, 2+2*len(x))
		buf[0] = '\\'
		buf[1] = 'x'
		for i, b := range x {
			buf[2+2*i] = hex[b>>4]
			buf[2+2*i+1] = hex[b&0xF]
		}
		return string(buf), false, nil
	case bool:
		if x {
			return "t", false, nil
		}
		return "f", false, nil
	case int:
		return strconv.FormatInt(int64(x), 10), false, nil
	case int32:
		return strconv.FormatInt(int64(x), 10), false, nil
	case int64:
		return strconv.FormatInt(x, 10), false, nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), false, nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), false, nil
	case uint64:
		return strconv.FormatUint(x, 10), false, nil
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), false, nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), false, nil
	default:
		return "", false, fmt.Errorf("pg2json: unsupported parameter type %T", v)
	}
}
