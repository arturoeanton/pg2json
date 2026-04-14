package protocol

// OID values are stable PostgreSQL constants from src/include/catalog/pg_type.dat.
// We only enumerate the ones with specialised encoders. Anything else falls
// back to the string encoder, which is the safe path for the text protocol.
type OID uint32

const (
	OIDBool        OID = 16
	OIDBytea       OID = 17
	OIDName        OID = 19
	OIDInt8        OID = 20
	OIDInt2        OID = 21
	OIDInt4        OID = 23
	OIDText        OID = 25
	OIDOID         OID = 26
	OIDJSON        OID = 114
	OIDFloat4      OID = 700
	OIDFloat8      OID = 701
	OIDBPChar      OID = 1042
	OIDVarchar     OID = 1043
	OIDDate        OID = 1082
	OIDTime        OID = 1083
	OIDTimestamp   OID = 1114
	OIDTimestampTZ OID = 1184
	OIDInterval    OID = 1186
	OIDTimeTZ      OID = 1266
	OIDNumeric     OID = 1700
	OIDUUID        OID = 2950
	OIDJSONB       OID = 3802

	// Array OIDs (pg_type.typelem resolves to the scalar OID above).
	OIDBoolArray        OID = 1000
	OIDInt2Array        OID = 1005
	OIDInt4Array        OID = 1007
	OIDTextArray        OID = 1009
	OIDBPCharArray      OID = 1014
	OIDVarcharArray     OID = 1015
	OIDInt8Array        OID = 1016
	OIDFloat4Array      OID = 1021
	OIDFloat8Array      OID = 1022
	OIDOIDArray         OID = 1028
	OIDByteaArray       OID = 1001
	OIDDateArray        OID = 1182
	OIDTimestampArray   OID = 1115
	OIDTimestampTZArray OID = 1185
	OIDUUIDArray        OID = 2951
	OIDJSONArray        OID = 199
	OIDJSONBArray       OID = 3807
	OIDNumericArray     OID = 1231
)

// ArrayElem returns the element OID for the given array OID, or 0 if the
// OID is not a recognised array type.
func ArrayElem(o OID) OID {
	switch o {
	case OIDBoolArray:
		return OIDBool
	case OIDInt2Array:
		return OIDInt2
	case OIDInt4Array:
		return OIDInt4
	case OIDInt8Array:
		return OIDInt8
	case OIDFloat4Array:
		return OIDFloat4
	case OIDFloat8Array:
		return OIDFloat8
	case OIDTextArray:
		return OIDText
	case OIDVarcharArray:
		return OIDVarchar
	case OIDBPCharArray:
		return OIDBPChar
	case OIDUUIDArray:
		return OIDUUID
	case OIDJSONArray:
		return OIDJSON
	case OIDJSONBArray:
		return OIDJSONB
	case OIDTimestampArray:
		return OIDTimestamp
	case OIDTimestampTZArray:
		return OIDTimestampTZ
	case OIDDateArray:
		return OIDDate
	case OIDOIDArray:
		return OIDOID
	case OIDByteaArray:
		return OIDBytea
	case OIDNumericArray:
		return OIDNumeric
	}
	return 0
}
