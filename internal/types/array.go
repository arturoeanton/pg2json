package types

import (
	"encoding/binary"

	"github.com/arturoeanton/pg2json/internal/jsonwriter"
	"github.com/arturoeanton/pg2json/internal/protocol"
)

// makeArrayBinaryEncoder builds a binary-array encoder parameterised by
// the element OID. Returns nil if the element type has no binary encoder.
//
// PostgreSQL array binary layout (see array_send in arrayfuncs.c):
//
//	int32 ndim
//	int32 flags        (bit 0: has nulls; otherwise reserved)
//	int32 elemOID
//	ndim × (int32 dimSize, int32 lBound)
//	flat elements in row-major order:
//	  int32 elemLen    (-1 for SQL NULL; else length of element bytes)
//	  elemLen bytes
//
// Multi-dimensional arrays become nested JSON arrays. Empty arrays
// (ndim=0) emit "[]".
func makeArrayBinaryEncoder(elemOID protocol.OID) Encoder {
	elemEnc := PickBinary(elemOID)
	if elemEnc == nil {
		return nil
	}
	return func(dst, raw []byte) []byte {
		if raw == nil {
			return jsonwriter.AppendNull(dst)
		}
		if len(raw) < 12 {
			return jsonwriter.AppendStringBytes(dst, raw)
		}
		ndim := int32(binary.BigEndian.Uint32(raw[0:4]))
		// flags (raw[4:8]) and declared elem OID (raw[8:12]) ignored — we
		// trust the plan's elemOID. PG confirms them server-side anyway.
		if ndim <= 0 {
			return append(dst, '[', ']')
		}
		hdrEnd := 12 + int(ndim)*8
		if len(raw) < hdrEnd {
			return jsonwriter.AppendStringBytes(dst, raw)
		}
		dims := make([]int, ndim)
		for i := int32(0); i < ndim; i++ {
			dims[i] = int(int32(binary.BigEndian.Uint32(raw[12+i*8 : 16+i*8])))
			if dims[i] < 0 {
				return jsonwriter.AppendStringBytes(dst, raw)
			}
		}
		body := raw[hdrEnd:]
		dst, _, ok := appendArrayDim(dst, body, dims, 0, elemEnc)
		if !ok {
			return jsonwriter.AppendStringBytes(dst, raw)
		}
		return dst
	}
}

// appendArrayDim recursively emits one dimension of the array. It returns
// the destination, the number of body bytes consumed, and ok=false if the
// body ran short (malformed).
func appendArrayDim(dst, body []byte, dims []int, depth int, elemEnc Encoder) ([]byte, int, bool) {
	dst = append(dst, '[')
	consumed := 0
	size := dims[depth]
	for i := 0; i < size; i++ {
		if i > 0 {
			dst = append(dst, ',')
		}
		if depth == len(dims)-1 {
			// leaf: read one element
			if len(body)-consumed < 4 {
				return dst, consumed, false
			}
			elemLen := int32(binary.BigEndian.Uint32(body[consumed : consumed+4]))
			consumed += 4
			if elemLen < 0 {
				dst = jsonwriter.AppendNull(dst)
				continue
			}
			if len(body)-consumed < int(elemLen) {
				return dst, consumed, false
			}
			dst = elemEnc(dst, body[consumed:consumed+int(elemLen)])
			consumed += int(elemLen)
		} else {
			var n int
			var ok bool
			dst, n, ok = appendArrayDim(dst, body[consumed:], dims, depth+1, elemEnc)
			if !ok {
				return dst, consumed + n, false
			}
			consumed += n
		}
	}
	dst = append(dst, ']')
	return dst, consumed, true
}
