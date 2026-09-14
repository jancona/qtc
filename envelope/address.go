package envelope

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Address is a 48-bit M17 address: a base-40 encoded callsign in the Standard
// range, a room in the Extended range (rooms spec §3.2), or one of the special
// values below. Only the low 48 bits are meaningful.
type Address uint64

// Landmarks in the M17 address space.
const (
	AddressZero    Address = 0              // reserved
	StandardMax    Address = 0xEE6B27FFFFFF // largest callsign address: 40^9 - 1
	ExtendedBase   Address = 0xEE6B28000000 // 40^9; start of the application range
	ExtendedEnd    Address = 0xFFFFFFFFFFFE // last Extended address
	Broadcast      Address = 0xFFFFFFFFFFFF // M17 "@ALL"; not supported for MSG
	RoomFirst      Address = ExtendedBase + 1
	RoomLast       Address = ExtendedBase + 40*40*40*40*40*40*40*40 - 1 // 0xF46108FFFFFF
	ReservedRooms  Address = RoomLast + 1                               // 0xF46109000000; rooms spec §3.3
	maxAddressText         = 9
	maxRoomName            = 8
)

const base40Alphabet = " ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-/."

// EncodeAddress base-40 encodes up to nine characters of callsign text: the
// M17 alphabet is space, A–Z, 0–9, '-', '/', and '.'; lower case is accepted
// and folded to upper. It applies no callsign policy beyond the alphabet and
// length, so device suffixes and unusual prefixes pass through unchanged.
func EncodeAddress(text string) (Address, error) {
	if text == "" {
		return 0, fmt.Errorf("envelope: empty address text")
	}
	if len(text) > maxAddressText {
		return 0, fmt.Errorf("envelope: address text %q longer than %d characters", text, maxAddressText)
	}
	var v uint64
	for i := len(text) - 1; i >= 0; i-- {
		c := text[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		d := strings.IndexByte(base40Alphabet, c)
		if d < 0 {
			return 0, fmt.Errorf("envelope: address text %q contains invalid character %q", text, text[i])
		}
		v = v*40 + uint64(d)
	}
	return Address(v), nil
}

// AddressFromBytes reads a big-endian 6-byte address.
func AddressFromBytes(b []byte) Address {
	_ = b[AddressLen-1]
	return Address(uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(binary.BigEndian.Uint32(b[2:6])))
}

// Bytes returns the big-endian 6-byte wire form.
func (a Address) Bytes() [AddressLen]byte {
	var out [AddressLen]byte
	a.put(out[:])
	return out
}

func (a Address) put(dst []byte) {
	_ = dst[AddressLen-1]
	dst[0] = byte(a >> 40)
	dst[1] = byte(a >> 32)
	binary.BigEndian.PutUint32(dst[2:6], uint32(a))
}

// IsStandard reports whether the address is a callsign (1 through StandardMax).
func (a Address) IsStandard() bool { return a > AddressZero && a <= StandardMax }

// IsExtended reports whether the address lies in the M17 Extended range.
func (a Address) IsExtended() bool { return a >= ExtendedBase && a <= ExtendedEnd }

// IsBroadcast reports whether the address is the M17 broadcast value.
func (a Address) IsBroadcast() bool { return a == Broadcast }

// IsRoom reports whether the address decodes to a valid room name.
func (a Address) IsRoom() bool {
	_, ok := a.RoomName()
	return ok
}

// Text base-40 decodes a Standard-range address to its callsign text, with
// no trailing padding. It fails for zero and for anything outside the
// Standard range.
func (a Address) Text() (string, error) {
	if !a.IsStandard() {
		return "", fmt.Errorf("envelope: address 0x%012X is not a callsign", uint64(a))
	}
	return base40Decode(uint64(a)), nil
}

func base40Decode(v uint64) string {
	var buf [maxAddressText]byte
	n := 0
	for v != 0 {
		buf[n] = base40Alphabet[v%40]
		v /= 40
		n++
	}
	return string(buf[:n])
}

// BaseCallsign returns the address text with any device suffix removed
// (architecture §3). It is the mechanical rule only: callers decide when an
// address is a node callsign that must be used whole. Non-callsign addresses
// yield "".
func (a Address) BaseCallsign() string {
	t, err := a.Text()
	if err != nil {
		return ""
	}
	base, _ := SplitCallsign(t)
	return base
}

// SplitCallsign splits callsign text into base callsign and device suffix.
// The base is everything before the first space or '-'; '/' is part of the
// callsign, not a separator. The suffix includes the separator, so the two
// parts concatenate back to the input.
func SplitCallsign(text string) (base, suffix string) {
	if i := strings.IndexAny(text, " -"); i >= 0 {
		return text[:i], text[i:]
	}
	return text, ""
}

// RoomAddress encodes a room name (rooms spec §3.1–3.2): one to eight
// characters of A–Z, 0–9, and '-', case-insensitive.
func RoomAddress(name string) (Address, error) {
	name = strings.ToUpper(name)
	if err := checkRoomName(name); err != nil {
		return 0, err
	}
	v, err := EncodeAddress(name)
	if err != nil {
		return 0, err
	}
	return ExtendedBase + v, nil
}

func checkRoomName(name string) error {
	if name == "" || len(name) > maxRoomName {
		return fmt.Errorf("envelope: room name %q must be 1 to %d characters", name, maxRoomName)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-':
		default:
			return fmt.Errorf("envelope: room name %q contains invalid character %q", name, name[i])
		}
	}
	return nil
}

// RoomName decodes a room address to its canonical (upper-case) name. It
// returns false for anything that is not a valid room: Standard addresses,
// the empty-name address, names using space, '/' or '.', names longer than
// eight characters, the reserved range of rooms spec §3.3, and broadcast.
func (a Address) RoomName() (string, bool) {
	if a < RoomFirst || a > RoomLast {
		return "", false
	}
	name := base40Decode(uint64(a - ExtendedBase))
	if checkRoomName(name) != nil {
		return "", false
	}
	return name, true
}

// String renders callsigns as text, rooms as "#NAME", broadcast as "@ALL",
// and anything else as hex.
func (a Address) String() string {
	if t, err := a.Text(); err == nil {
		return t
	}
	if name, ok := a.RoomName(); ok {
		return "#" + name
	}
	if a.IsBroadcast() {
		return "@ALL"
	}
	return fmt.Sprintf("0x%012X", uint64(a))
}
