package envelope

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ID is a derived message ID: the first 8 bytes of SHA-256 over the MSG
// signing input (§4.3). It is never transmitted inside a MSG.
type ID [IDLen]byte

// IDFromBytes copies an 8-byte ID.
func IDFromBytes(b []byte) ID {
	var id ID
	copy(id[:], b)
	return id
}

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Errors returned by Parse and the signature functions. Parse errors wrap
// ErrInvalid so callers can test for "malformed" without matching text.
var (
	ErrInvalid        = errors.New("envelope: invalid envelope")
	ErrUnsigned       = errors.New("envelope: not signed")
	ErrBadSignature   = errors.New("envelope: signature does not verify")
	ErrNotSignable    = errors.New("envelope: no signature is defined for this packet type")
	ErrAlreadySigned  = errors.New("envelope: already signed")
	ErrUnknownVersion = errors.New("envelope: unknown version")
)

// Envelope is a parsed, validated, immutable MSG, RCPT, or ROOM payload
// (type byte through the end of any signature, CRC excluded). The zero value
// is not valid; obtain one from Parse, a Build* function, or UnmarshalJSON.
type Envelope struct {
	raw []byte
	id  ID // MSG only; zero otherwise
}

// Parse validates b and returns an Envelope over a private copy of it. It
// rejects unknown types and versions, truncated headers, oversized payloads,
// a SIGNED flag without room for a signature, and a ROOM count that does not
// match the payload. Reserved flag bits are preserved, never rejected.
func Parse(b []byte) (*Envelope, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrInvalid)
	}
	if len(b) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte packet", ErrInvalid, len(b), MaxPayload)
	}
	t := PacketType(b[0])
	var hdr int
	switch t {
	case TypeMSG:
		hdr = MsgHeaderLen
	case TypeRCPT:
		hdr = RcptHeaderLen
	case TypeROOM:
		hdr = RoomHeaderLen
	default:
		return nil, fmt.Errorf("%w: packet type %s", ErrInvalid, t)
	}
	if len(b) < hdr {
		return nil, fmt.Errorf("%w: %s payload of %d bytes is shorter than the %d-byte header", ErrInvalid, t, len(b), hdr)
	}
	if b[1] != Version0 {
		return nil, fmt.Errorf("%w: %s version %d", ErrUnknownVersion, t, b[1])
	}
	switch t {
	case TypeMSG, TypeRCPT:
		if b[2]&FlagSigned != 0 && len(b) < hdr+SignatureLen {
			return nil, fmt.Errorf("%w: %s has SIGNED set but only %d bytes after the header", ErrInvalid, t, len(b)-hdr)
		}
	case TypeROOM:
		n := int(b[8])
		if len(b) < RoomHeaderLen+n*AddressLen {
			return nil, fmt.Errorf("%w: ROOM count %d needs %d bytes, have %d", ErrInvalid, n, n*AddressLen, len(b)-RoomHeaderLen)
		}
	}
	e := &Envelope{raw: append([]byte(nil), b...)}
	if t == TypeMSG {
		sum := sha256.Sum256(e.SigningInput())
		copy(e.id[:], sum[:IDLen])
	}
	return e, nil
}

// Bytes returns a copy of the wire bytes.
func (e *Envelope) Bytes() []byte { return append([]byte(nil), e.raw...) }

// Len is the payload length in bytes.
func (e *Envelope) Len() int { return len(e.raw) }

// Type is the packet type byte.
func (e *Envelope) Type() PacketType { return PacketType(e.raw[0]) }

// Version is the envelope version, always Version0 for a parsed envelope.
func (e *Envelope) Version() byte { return e.raw[1] }

// Flags is the raw flags byte, reserved bits included.
func (e *Envelope) Flags() byte { return e.raw[2] }

// Signed reports whether the SIGNED flag is set. For ROOM the flag is
// reserved and carries no signature; see Signature.
func (e *Envelope) Signed() bool { return e.raw[2]&FlagSigned != 0 }

// Source is the sending address. ROOM packets carry no addresses (they come
// from the LSF) and return AddressZero.
func (e *Envelope) Source() Address {
	if e.Type() == TypeROOM {
		return AddressZero
	}
	return AddressFromBytes(e.raw[3:9])
}

// Destination is the receiving address, or AddressZero for ROOM.
func (e *Envelope) Destination() Address {
	if e.Type() == TypeROOM {
		return AddressZero
	}
	return AddressFromBytes(e.raw[9:15])
}

// Timestamp is the envelope's Unix time in seconds; 0 means unknown.
func (e *Envelope) Timestamp() uint32 {
	switch e.Type() {
	case TypeMSG:
		return binary.BigEndian.Uint32(e.raw[15:19])
	case TypeRCPT:
		return binary.BigEndian.Uint32(e.raw[24:28])
	default:
		return binary.BigEndian.Uint32(e.raw[4:8])
	}
}

// ID is the derived message ID of a MSG (§4.3), computed once at parse time.
// It is the zero ID for RCPT and ROOM, which have no ID of their own.
func (e *Envelope) ID() ID { return e.id }

// sigStart is the offset of the signature, or len(raw) if there is none.
func (e *Envelope) sigStart() int {
	if e.Signed() && e.Type() != TypeROOM {
		return len(e.raw) - SignatureLen
	}
	return len(e.raw)
}

// Signature returns the 64-byte raw r‖s signature of a signed MSG or RCPT.
func (e *Envelope) Signature() ([]byte, bool) {
	s := e.sigStart()
	if s == len(e.raw) {
		return nil, false
	}
	return append([]byte(nil), e.raw[s:]...), true
}

// SigningInput is the canonical byte string that is hashed for the message
// ID and signed: Version ‖ Flags with SIGNED cleared ‖ everything through the
// end of the body or note, with the type byte and signature excluded (§4.3,
// §5.4). It is nil for ROOM, which defines no signature.
func (e *Envelope) SigningInput() []byte {
	if e.Type() == TypeROOM {
		return nil
	}
	in := append([]byte(nil), e.raw[1:e.sigStart()]...)
	in[1] &^= FlagSigned
	return in
}

// StripSignature returns an equivalent unsigned envelope: the signature is
// removed and the SIGNED flag cleared, which leaves the message ID unchanged.
// An unsigned envelope is returned as is.
func (e *Envelope) StripSignature() *Envelope {
	if !e.Signed() || e.Type() == TypeROOM {
		return e
	}
	raw := append([]byte(nil), e.raw[:e.sigStart()]...)
	raw[2] &^= FlagSigned
	return &Envelope{raw: raw, id: e.id}
}

// Msg returns the MSG view of the envelope.
func (e *Envelope) Msg() (Msg, bool) { return Msg{e}, e.Type() == TypeMSG }

// Rcpt returns the RCPT view of the envelope.
func (e *Envelope) Rcpt() (Rcpt, bool) { return Rcpt{e}, e.Type() == TypeRCPT }

// Room returns the ROOM view of the envelope.
func (e *Envelope) Room() (Room, bool) { return Room{e}, e.Type() == TypeROOM }

// String is the one-line form used in logs.
func (e *Envelope) String() string {
	var sb strings.Builder
	sb.WriteString(e.Type().String())
	switch e.Type() {
	case TypeMSG:
		m := Msg{e}
		fmt.Fprintf(&sb, " %s→%s id=%s ts=%d ttl=%d nonce=0x%04X flags=0x%02X body=%q",
			e.Source(), e.Destination(), e.id, e.Timestamp(), m.TTL(), m.Nonce(), e.Flags(), m.Body())
	case TypeRCPT:
		r := Rcpt{e}
		fmt.Fprintf(&sb, " %s→%s %s id=%s ts=%d heard=%d flags=0x%02X",
			e.Source(), e.Destination(), r.Status(), r.MessageID(), e.Timestamp(), r.LastHeard(), e.Flags())
		if n := r.Note(); n != "" {
			fmt.Fprintf(&sb, " note=%q", n)
		}
	case TypeROOM:
		r := Room{e}
		fmt.Fprintf(&sb, " %s ts=%d flags=0x%02X rooms=[", r.Op(), e.Timestamp(), e.Flags())
		for i, a := range r.Rooms() {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(a.String())
		}
		sb.WriteByte(']')
		if n := r.Note(); n != "" {
			fmt.Fprintf(&sb, " note=%q", n)
		}
	}
	if e.Signed() && e.Type() != TypeROOM {
		sb.WriteString(" signed")
	}
	return sb.String()
}

// MarshalJSON emits the raw bytes as a base64 JSON string.
func (e *Envelope) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(e.raw))
}

// UnmarshalJSON parses and validates a base64 JSON string.
func (e *Envelope) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("envelope: bad base64: %w", err)
	}
	p, err := Parse(raw)
	if err != nil {
		return err
	}
	*e = *p
	return nil
}
