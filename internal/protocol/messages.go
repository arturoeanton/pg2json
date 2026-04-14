// Package protocol holds PostgreSQL v3 frontend/backend message codes and a
// few small helpers. The codes are mirrored from src/include/libpq/protocol.h
// in the PostgreSQL source tree.
package protocol

// Backend message types (server -> client).
const (
	MsgAuthentication       byte = 'R'
	MsgBackendKeyData       byte = 'K'
	MsgBindComplete         byte = '2'
	MsgCloseComplete        byte = '3'
	MsgCommandComplete      byte = 'C'
	MsgDataRow              byte = 'D'
	MsgEmptyQueryResponse   byte = 'I'
	MsgErrorResponse        byte = 'E'
	MsgNoData               byte = 'n'
	MsgNoticeResponse       byte = 'N'
	MsgNotificationResponse byte = 'A'
	MsgParameterDescription byte = 't'
	MsgParameterStatus      byte = 'S'
	MsgParseComplete        byte = '1'
	MsgPortalSuspended      byte = 's'
	MsgReadyForQuery        byte = 'Z'
	MsgRowDescription       byte = 'T'
	MsgNegotiateProtocol    byte = 'v'
)

// Frontend message types (client -> server).
const (
	MsgBind     byte = 'B'
	MsgClose    byte = 'C'
	MsgDescribe byte = 'D'
	MsgExecute  byte = 'E'
	MsgFlush    byte = 'H'
	MsgParse    byte = 'P'
	MsgQuery    byte = 'Q'
	MsgSync     byte = 'S'
	MsgTerm     byte = 'X'
	MsgPassword byte = 'p' // also SASL{Initial,}Response
)

// Authentication request subtypes (the int32 immediately following
// the length in an 'R' message).
const (
	AuthOK                = 0
	AuthCleartextPassword = 3
	AuthMD5Password       = 5
	AuthSASL              = 10
	AuthSASLContinue      = 11
	AuthSASLFinal         = 12
)

// Protocol version: major 3, minor 0. Newer minors exist but 3.0 is what
// every server understands and is what we actually need.
const ProtocolVersion3 = (3 << 16) | 0

// Format codes for Bind/RowDescription.
const (
	FormatText   int16 = 0
	FormatBinary int16 = 1
)
