package envelope

import (
	"encoding/binary"
	"fmt"
)

// Room is the ROOM view of an Envelope (rooms spec §5). Obtain it from
// Envelope.Room. ROOM packets carry no addresses of their own and no
// signature is defined for them in this version.
type Room struct{ e *Envelope }

// Envelope returns the underlying envelope.
func (r Room) Envelope() *Envelope { return r.e }

// ROOM offsets (rooms spec §5.1).
const (
	roomOffOp    = 4
	roomOffCount = 9
)

// Op is the operation (rooms spec §5.2).
func (r Room) Op() RoomOp { return RoomOp(r.e.raw[roomOffOp]) }

// Count is the number of room addresses.
func (r Room) Count() int { return int(r.e.raw[roomOffCount]) }

// Rooms returns the listed room addresses.
func (r Room) Rooms() []Address {
	n := r.Count()
	out := make([]Address, n)
	for i := range out {
		off := RoomHeaderLen + i*AddressLen
		out[i] = AddressFromBytes(r.e.raw[off : off+AddressLen])
	}
	return out
}

// Note is the optional text following the rooms list; replies only.
func (r Room) Note() string { return string(r.e.raw[RoomHeaderLen+r.Count()*AddressLen:]) }

// BuildRoom produces a ROOM packet with flags 0.
func BuildRoom(op RoomOp, timestamp uint32, rooms []Address, note string) (*Envelope, error) {
	if len(rooms) > 255 {
		return nil, fmt.Errorf("envelope: %d rooms exceeds 255", len(rooms))
	}
	if n := RoomHeaderLen + len(rooms)*AddressLen + len(note); n > MaxPayload {
		return nil, fmt.Errorf("envelope: ROOM payload of %d bytes exceeds %d", n, MaxPayload)
	}
	raw := make([]byte, RoomHeaderLen, RoomHeaderLen+len(rooms)*AddressLen+len(note))
	raw[offType] = byte(TypeQTC)
	raw[offKind] = byte(KindROOM)
	raw[offVersion] = Version0
	raw[roomOffOp] = byte(op)
	binary.BigEndian.PutUint32(raw[5:9], timestamp)
	raw[roomOffCount] = byte(len(rooms))
	for _, a := range rooms {
		var b [AddressLen]byte
		a.put(b[:])
		raw = append(raw, b[:]...)
	}
	raw = append(raw, note...)
	return Parse(raw)
}
