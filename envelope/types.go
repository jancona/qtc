// Package envelope implements the QTC message envelope: the MSG and RCPT
// packet types from docs/qtc-envelope.md and the ROOM control packet from
// docs/qtc-rooms.md §5.
//
// An Envelope is an immutable byte string. It is parsed once, validated, and
// then read through accessors; nothing here rebuilds an envelope from fields,
// so the bytes a node receives are the bytes it stores and forwards. New
// envelopes come from the Build* functions, which produce the bytes once and
// return them parsed.
//
// The package is standard library only.
package envelope

// PacketType is the M17 packet mode type byte. QTC's values are
// provisional (envelope spec §3) and are kept here, in one place, so they are
// easy to change. 0x07 is TLE in M17 3.0.0 and must not be used.
type PacketType byte

const (
	// TypeSMS is the M17 SMS packet type, translated at the node boundary (§6).
	TypeSMS PacketType = 0x05
	// TypeMSG is a message envelope (§4).
	TypeMSG PacketType = 0x08
	// TypeRCPT is a receipt (§5).
	TypeRCPT PacketType = 0x09
	// TypeROOM is a room control packet (rooms spec §5).
	TypeROOM PacketType = 0x0A
)

func (t PacketType) String() string {
	switch t {
	case TypeSMS:
		return "SMS"
	case TypeMSG:
		return "MSG"
	case TypeRCPT:
		return "RCPT"
	case TypeROOM:
		return "ROOM"
	}
	return "0x" + hexByte(byte(t))
}

// Version0 is the only envelope version this package understands.
const Version0 byte = 0x00

// Flag bits shared by MSG and RCPT (§4.2, §5.1). ROOM carries the same byte
// with only SIGNED defined, and no signature is defined for it yet.
const (
	FlagSigned  byte = 0x01
	FlagRcptReq byte = 0x02
)

// Sizes, in bytes, of the fixed parts of each layout.
const (
	MaxPayload    = 823 // M17 packet mode payload including the type byte, excluding the CRC
	AddressLen    = 6
	IDLen         = 8
	SignatureLen  = 64
	MsgHeaderLen  = 23
	RcptHeaderLen = 32
	RoomHeaderLen = 9

	MaxBodyUnsigned = MaxPayload - MsgHeaderLen                // 800
	MaxBodySigned   = MaxPayload - MsgHeaderLen - SignatureLen // 736
)

// TTL sentinels (§4.4).
const (
	TTLLiveOnly uint16 = 0      // must not be stored
	TTLDefault  uint16 = 0xFFFF // node applies its configured default
)

// Status is an RCPT status code (§5.2).
type Status byte

const (
	StatusQueued      Status = 0x00
	StatusTransmitted Status = 0x01
	StatusDelivered   Status = 0x02
	StatusExpired     Status = 0x04
	StatusRejected    Status = 0x05
)

func (s Status) String() string {
	switch s {
	case StatusQueued:
		return "QUEUED"
	case StatusTransmitted:
		return "TRANSMITTED"
	case StatusDelivered:
		return "DELIVERED"
	case StatusExpired:
		return "EXPIRED"
	case StatusRejected:
		return "REJECTED"
	}
	return "STATUS(0x" + hexByte(byte(s)) + ")"
}

// RoomOp is a ROOM packet operation (rooms spec §5.2).
type RoomOp byte

const (
	OpJoin    RoomOp = 0x00
	OpLeave   RoomOp = 0x01
	OpList    RoomOp = 0x02
	OpOK      RoomOp = 0x80
	OpRefused RoomOp = 0x81
)

// IsReply reports whether the op is a node-to-client reply.
func (op RoomOp) IsReply() bool { return op&0x80 != 0 }

func (op RoomOp) String() string {
	switch op {
	case OpJoin:
		return "JOIN"
	case OpLeave:
		return "LEAVE"
	case OpList:
		return "LIST"
	case OpOK:
		return "OK"
	case OpRefused:
		return "REFUSED"
	}
	return "OP(0x" + hexByte(byte(op)) + ")"
}

const hexDigits = "0123456789abcdef"

func hexByte(b byte) string {
	return string([]byte{hexDigits[b>>4], hexDigits[b&0x0F]})
}
