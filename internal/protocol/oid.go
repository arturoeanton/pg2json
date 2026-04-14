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
	OIDTimestamp  OID = 1114
	OIDTimestampTZ OID = 1184
	OIDInterval    OID = 1186
	OIDTimeTZ      OID = 1266
	OIDNumeric     OID = 1700
	OIDUUID        OID = 2950
	OIDJSONB       OID = 3802
)
