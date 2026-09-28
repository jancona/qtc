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
	ErrUnknownKind    = errors.New("envelope: unknown kind")
	ErrUnsigned       = errors.New("envelope: not signed")
	ErrBadSignature   = errors.New("envelope: signature does not verify")
	ErrNotSignable    = errors.New("envelope: no signature is defined for this kind")
	ErrAlreadySigned  = errors.New("envelope: already signed")
	ErrUnknownVersion = errors.New("envelope: unknown version")
)

// Offsets common to every kind (§3).
const (
	offType    = 0
	offKind    = 1
	offVersion = 2
	offFlags   = 3
)

// Envelope is a parsed, validated, immutable QTC payload (type byte through
// the end of any signature, CRC excluded) of any kind. The zero value is not
// valid; obtain one from Parse, a Build* function, or UnmarshalJSON.
type Envelope struct {
	raw []byte
	id  ID // MSG only; zero otherwise
}

// Parse validates b and returns an Envelope over a private copy of it. It
// rejects anything but the QTC packet type, unknown kinds (wrapping
// ErrUnknownKind, which a receiver ignores) and versions, truncated headers,
// oversized payloads, a SIGNED flag without room for a signature, and counts
// that do not match the payload. Reserved flag bits are preserved, never
// rejected.
func Parse(b []byte) (*Envelope, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("%w: payload of %d bytes", ErrInvalid, len(b))
	}
	if len(b) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte packet", ErrInvalid, len(b), MaxPayload)
	}
	if t := PacketType(b[offType]); t != TypeQTC {
		return nil, fmt.Errorf("%w: packet type %s", ErrInvalid, t)
	}
	k := Kind(b[offKind])
	var hdr int
	switch k {
	case KindMSG:
		hdr = MsgHeaderLen
	case KindRCPT:
		hdr = RcptHeaderLen
	case KindROOM:
		hdr = RoomHeaderLen
	case KindSYNC:
		hdr = SyncHeaderLen
	case KindACK:
		hdr = AckHeaderLen
	default:
		return nil, fmt.Errorf("%w: %w %s", ErrInvalid, ErrUnknownKind, k)
	}
	if len(b) < hdr {
		return nil, fmt.Errorf("%w: %s payload of %d bytes is shorter than the %d-byte header", ErrInvalid, k, len(b), hdr)
	}
	if b[offVersion] != Version0 {
		return nil, fmt.Errorf("%w: %s version %d", ErrUnknownVersion, k, b[offVersion])
	}
	switch k {
	case KindMSG, KindRCPT:
		if b[offFlags]&FlagSigned != 0 && len(b) < hdr+SignatureLen {
			return nil, fmt.Errorf("%w: %s has SIGNED set but only %d bytes after the header", ErrInvalid, k, len(b)-hdr)
		}
	case KindROOM:
		n := int(b[roomOffCount])
		if len(b) < RoomHeaderLen+n*AddressLen {
			return nil, fmt.Errorf("%w: ROOM count %d needs %d bytes, have %d", ErrInvalid, n, n*AddressLen, len(b)-RoomHeaderLen)
		}
	case KindSYNC:
		if err := checkSyncTail(SyncOp(b[syncOffOp]), int(b[syncOffCount]), b[SyncHeaderLen:]); err != nil {
			return nil, err
		}
	case KindACK:
		n := int(b[ackOffCount])
		if n == 0 || len(b) != AckHeaderLen+n*IDLen {
			return nil, fmt.Errorf("%w: ACK count %d with %d bytes of IDs", ErrInvalid, n, len(b)-AckHeaderLen)
		}
	}
	e := &Envelope{raw: append([]byte(nil), b...)}
	if k == KindMSG {
		sum := sha256.Sum256(e.idInput())
		copy(e.id[:], sum[:IDLen])
	}
	return e, nil
}

// Bytes returns a copy of the wire bytes.
func (e *Envelope) Bytes() []byte { return append([]byte(nil), e.raw...) }

// Len is the payload length in bytes.
func (e *Envelope) Len() int { return len(e.raw) }

// Kind is the kind byte.
func (e *Envelope) Kind() Kind { return Kind(e.raw[offKind]) }

// Version is the envelope version, always Version0 for a parsed envelope.
func (e *Envelope) Version() byte { return e.raw[offVersion] }

// Flags is the raw flags byte, reserved bits included.
func (e *Envelope) Flags() byte { return e.raw[offFlags] }

// signable reports whether a signature is defined for the kind.
func (e *Envelope) signable() bool { k := e.Kind(); return k == KindMSG || k == KindRCPT }

// Signed reports whether the SIGNED flag is set. For kinds with no signature
// defined the flag is reserved and carries none; see Signature.
func (e *Envelope) Signed() bool { return e.raw[offFlags]&FlagSigned != 0 }

// Source is the sending address. Control kinds (ROOM, SYNC, ACK) carry no
// addresses of their own (they come from the LSF) and return AddressZero.
func (e *Envelope) Source() Address {
	if !e.signable() {
		return AddressZero
	}
	return AddressFromBytes(e.raw[4:10])
}

// Destination is the receiving address, or AddressZero for control kinds.
func (e *Envelope) Destination() Address {
	if !e.signable() {
		return AddressZero
	}
	return AddressFromBytes(e.raw[10:16])
}

// Timestamp is the envelope's Unix time in seconds; 0 means unknown, and
// SYNC and ACK have none.
func (e *Envelope) Timestamp() uint32 {
	switch e.Kind() {
	case KindMSG:
		return binary.BigEndian.Uint32(e.raw[16:20])
	case KindRCPT:
		return binary.BigEndian.Uint32(e.raw[25:29])
	case KindROOM:
		return binary.BigEndian.Uint32(e.raw[5:9])
	}
	return 0
}

// ID is the derived message ID of a MSG (§4.3), computed once at parse time.
// It is the zero ID for every other kind, which have no ID of their own.
func (e *Envelope) ID() ID { return e.id }

// sigStart is the offset of the signature, or len(raw) if there is none.
func (e *Envelope) sigStart() int {
	if e.Signed() && e.signable() {
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

// idInput is what the message ID hashes (§4.3): Version ‖ Flags with SIGNED
// cleared ‖ everything through the end of the body or note, with the type,
// kind, and signature excluded.
func (e *Envelope) idInput() []byte {
	in := append([]byte(nil), e.raw[offVersion:e.sigStart()]...)
	in[1] &^= FlagSigned
	return in
}

// SigningInput is the byte string a signature covers (§3, §4.6, §5.4):
// "QTC" ‖ Kind ‖ the message ID input. It is nil for kinds with no
// signature defined.
func (e *Envelope) SigningInput() []byte {
	if !e.signable() {
		return nil
	}
	in := append([]byte(SigningContext), byte(e.Kind()))
	return append(in, e.idInput()...)
}

// StripSignature returns an equivalent unsigned envelope: the signature is
// removed and the SIGNED flag cleared, which leaves the message ID unchanged.
// An unsigned envelope is returned as is.
func (e *Envelope) StripSignature() *Envelope {
	if !e.Signed() || !e.signable() {
		return e
	}
	raw := append([]byte(nil), e.raw[:e.sigStart()]...)
	raw[offFlags] &^= FlagSigned
	return &Envelope{raw: raw, id: e.id}
}

// Msg returns the MSG view of the envelope.
func (e *Envelope) Msg() (Msg, bool) { return Msg{e}, e.Kind() == KindMSG }

// Rcpt returns the RCPT view of the envelope.
func (e *Envelope) Rcpt() (Rcpt, bool) { return Rcpt{e}, e.Kind() == KindRCPT }

// Room returns the ROOM view of the envelope.
func (e *Envelope) Room() (Room, bool) { return Room{e}, e.Kind() == KindROOM }

// Sync returns the SYNC view of the envelope.
func (e *Envelope) Sync() (Sync, bool) { return Sync{e}, e.Kind() == KindSYNC }

// Ack returns the ACK view of the envelope.
func (e *Envelope) Ack() (Ack, bool) { return Ack{e}, e.Kind() == KindACK }

// String is the one-line form used in logs.
func (e *Envelope) String() string {
	var sb strings.Builder
	sb.WriteString(e.Kind().String())
	switch e.Kind() {
	case KindMSG:
		m := Msg{e}
		fmt.Fprintf(&sb, " %s→%s id=%s ts=%d ttl=%d nonce=0x%04X flags=0x%02X body=%q",
			e.Source(), e.Destination(), e.id, e.Timestamp(), m.TTL(), m.Nonce(), e.Flags(), m.Body())
	case KindRCPT:
		r := Rcpt{e}
		fmt.Fprintf(&sb, " %s→%s %s id=%s ts=%d heard=%d flags=0x%02X",
			e.Source(), e.Destination(), r.Status(), r.MessageID(), e.Timestamp(), r.LastHeard(), e.Flags())
		if n := r.Note(); n != "" {
			fmt.Fprintf(&sb, " note=%q", n)
		}
	case KindROOM:
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
	case KindSYNC:
		y := Sync{e}
		fmt.Fprintf(&sb, " %s flags=0x%02X cursor=%d skip=%d count=%d remaining=%d", y.Op(), e.Flags(), y.Cursor(), y.Skip(), y.Count(), y.Remaining())
		if n := y.Note(); n != "" {
			fmt.Fprintf(&sb, " note=%q", n)
		}
	case KindACK:
		sb.WriteString(" ids=[")
		for i, id := range (Ack{e}).IDs() {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(id.String())
		}
		sb.WriteByte(']')
	}
	if e.Signed() && e.signable() {
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

// StoreID is the key under which a mailbox stores the envelope (node protocol
// §5): the message ID for a MSG, and for RCPT and ROOM, which have no ID of
// their own, the first 8 bytes of SHA-256 over the envelope bytes.
func (e *Envelope) StoreID() ID {
	if e.Kind() == KindMSG {
		return e.id
	}
	sum := sha256.Sum256(e.raw)
	return IDFromBytes(sum[:IDLen])
}
