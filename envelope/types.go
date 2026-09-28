// Package envelope implements the QTC packet type: the MSG and RCPT kinds
// from docs/qtc-envelope.md, the ROOM control kind from docs/qtc-rooms.md §5,
// and the SYNC and ACK kinds from docs/qtc-client.md.
//
// An Envelope is an immutable byte string. It is parsed once, validated, and
// then read through accessors; nothing here rebuilds an envelope from fields,
// so the bytes a node receives are the bytes it stores and forwards. New
// envelopes come from the Build* functions, which produce the bytes once and
// return them parsed.
//
// The package is standard library only.
package envelope

// PacketType is the M17 packet mode type byte. QTC's value is provisional
// (envelope spec §3) and is kept here, in one place, so it is easy to change.
// 0x07 is TLE in M17 3.0.0 and must not be used.
type PacketType byte

const (
	// TypeSMS is the M17 SMS packet type, translated at the node boundary (§6).
	TypeSMS PacketType = 0x05
	// TypeQTC is the QTC packet type; the Kind byte follows it (§3).
	TypeQTC PacketType = 0x08
)

func (t PacketType) String() string {
	switch t {
	case TypeSMS:
		return "SMS"
	case TypeQTC:
		return "QTC"
	}
	return "0x" + hexByte(byte(t))
}

// Kind is the byte after TypeQTC that says what kind of QTC packet it is
// (§3). SYNC and ACK are defined in docs/qtc-client.md.
type Kind byte

const (
	KindMSG  Kind = 0x01 // message envelope (§4)
	KindRCPT Kind = 0x02 // receipt (§5)
	KindROOM Kind = 0x03 // room control (rooms spec §5)
	KindSYNC Kind = 0x04 // sync, notify, fetch, summary (client spec §5)
	KindACK  Kind = 0x05 // hop acknowledgement (client spec §4.1)
)

// Stored reports whether envelopes of this kind are stored in mailboxes and
// carried between nodes. SYNC and ACK are single-hop between a device and
// its node and never are.
func (k Kind) Stored() bool { return k == KindMSG || k == KindRCPT || k == KindROOM }

func (k Kind) String() string {
	switch k {
	case KindMSG:
		return "MSG"
	case KindRCPT:
		return "RCPT"
	case KindROOM:
		return "ROOM"
	case KindSYNC:
		return "SYNC"
	case KindACK:
		return "ACK"
	}
	return "KIND(0x" + hexByte(byte(k)) + ")"
}

// SigningContext prefixes every QTC signing input, ahead of the Kind (§3).
const SigningContext = "QTC"

// Version0 is the only envelope version this package understands.
const Version0 byte = 0x00

// Flag bits shared by MSG and RCPT (§4.2, §5.1). The other kinds carry the
// same byte with only SIGNED defined, and no signature is defined for them
// yet.
const (
	FlagSigned  byte = 0x01
	FlagRcptReq byte = 0x02
)

// Sizes, in bytes, of the fixed parts of each layout.
const (
	MaxPayload    = 823 // M17 packet mode payload including the type byte, excluding the CRC
	AddressLen    = 6
	IDLen         = 8
	ShortIDLen    = 4 // message ID prefix in room summaries and FETCH (client spec §6.1)
	SignatureLen  = 64
	MsgHeaderLen  = 24
	RcptHeaderLen = 33
	RoomHeaderLen = 10
	SyncHeaderLen = 14
	AckHeaderLen  = 5

	MaxBodyUnsigned = MaxPayload - MsgHeaderLen                // 799
	MaxBodySigned   = MaxPayload - MsgHeaderLen - SignatureLen // 735
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

// SyncOp is a SYNC operation (client spec §5.1).
type SyncOp byte

const (
	SyncRequest SyncOp = 0x00
	SyncFetch   SyncOp = 0x01
	SyncPage    SyncOp = 0x80
	SyncNotify  SyncOp = 0x81
	SyncRefused SyncOp = 0x82
	SyncSummary SyncOp = 0x83
)

// SYNC REQUEST flags (client spec §5.1).
const (
	SyncFlagAll  byte = 0x01 // include messages this device already acknowledged
	SyncFlagSent byte = 0x02 // include messages the callsign sent
)

// IsReply reports whether the op is sent by a node.
func (op SyncOp) IsReply() bool { return op&0x80 != 0 }

func (op SyncOp) String() string {
	switch op {
	case SyncRequest:
		return "REQUEST"
	case SyncFetch:
		return "FETCH"
	case SyncPage:
		return "PAGE"
	case SyncNotify:
		return "NOTIFY"
	case SyncRefused:
		return "REFUSED"
	case SyncSummary:
		return "SUMMARY"
	}
	return "OP(0x" + hexByte(byte(op)) + ")"
}

func hexByte(b byte) string {
	return string([]byte{hexDigits[b>>4], hexDigits[b&0x0F]})
}
