package qtcd

import (
	"fmt"

	"github.com/jancona/m17"
	"github.com/jancona/qtc/envelope"
)

// M17_inet framing for the client face, built on the m17 root package:
// just enough to forward datagrams, read source and destination addresses,
// and build SMS packets.

// M17_inet datagram magics, from m17.
const (
	magicCONN = m17.MagicCONN
	magicLSTN = m17.MagicLSTN
	magicACKN = m17.MagicACKN
	magicNACK = m17.MagicNACK
	magicPING = m17.MagicPING
	magicPONG = m17.MagicPONG
	magicDISC = m17.MagicDISC
	magicM17S = m17.MagicM17Stream
	magicM17P = m17.MagicM17Packet
)

const (
	lsfLen      = m17.LSFLen
	streamDgLen = 54 // magic, stream ID, LSD, frame number, payload, CRC
)

func encodedCallsign(a envelope.Address) m17.EncodedCallsign {
	return m17.EncodedCallsign(a.Bytes())
}

// buildLSF returns a packet-mode LSF with empty META, encoded as m17 does
// for packets (TYPE 0, CAN 0).
func buildLSF(dst, src envelope.Address) m17.LSF {
	lsf := m17.NewEmptyLSF()
	lsf.Dst = encodedCallsign(dst)
	lsf.Src = encodedCallsign(src)
	lsf.CalcCRC()
	return lsf
}

// buildPacketDatagram frames a packet payload (type byte and contents,
// without CRC) as an M17_inet "M17P" datagram.
func buildPacketDatagram(dst, src envelope.Address, payload []byte) []byte {
	lsf := buildLSF(dst, src)
	p := m17.Packet{LSF: &lsf, Type: m17.PacketType(payload[0]), Payload: payload[1:]}
	p.CalcCRC()
	out := make([]byte, 0, 4+lsfLen+len(payload)+2)
	out = append(out, magicM17P...)
	return append(out, p.ToBytes()...)
}

// packetFrame is a parsed "M17P" datagram.
type packetFrame struct {
	dst, src envelope.Address
	typ      envelope.PacketType
	payload  []byte // type byte through the end of the contents, CRC stripped
	relayed  bool   // the LSF says a gateway or reflector relayed it (see relayed)
}

// relayed reports whether an LSF marks its transmission as relayed rather
// than sent by the radio named as its source: Extended Callsign Data in
// META naming a reflector (slot 2), or someone other than the source in
// slot 1. m17-gateway marks everything it transmits from the network this
// way: packets with its own callsign and the reflector, voice with the
// source and the reflector. A repeater's local repeat (the source in slot
// 1, slot 2 empty) is still the radio's own transmission.
func relayed(lsf *m17.LSF) bool {
	e := lsf.ECD()
	if e == nil {
		return false
	}
	var zero m17.EncodedCallsign
	return *e.Callsign2 != zero || (*e.Callsign1 != zero && *e.Callsign1 != lsf.Src)
}

// parsePacketDatagram parses an "M17P" datagram, checking both CRCs.
// QTC's and SMS's packet types are all single bytes.
func parsePacketDatagram(b []byte) (packetFrame, error) {
	var f packetFrame
	if len(b) < 4+lsfLen+1+2 {
		return f, fmt.Errorf("packet datagram of %d bytes too short", len(b))
	}
	p := m17.NewPacketFromBytes(b[4:])
	if !p.LSF.CheckCRC() {
		return f, fmt.Errorf("packet LSF CRC mismatch")
	}
	if !p.CheckCRC() {
		return f, fmt.Errorf("packet payload CRC mismatch")
	}
	if p.Type > 0x7F {
		return f, fmt.Errorf("packet type %#x is not a single byte", uint32(p.Type))
	}
	f.dst = envelope.AddressFromBytes(p.LSF.Dst[:])
	f.src = envelope.AddressFromBytes(p.LSF.Src[:])
	f.typ = envelope.PacketType(p.Type)
	f.payload = append([]byte{byte(p.Type)}, p.Payload...)
	f.relayed = relayed(p.LSF)
	return f, nil
}

// streamAddrs reads the destination and source from a stream frame, and
// whether it was relayed.
func streamAddrs(b []byte) (dst, src envelope.Address, isRelayed, ok bool) {
	sd, err := m17.NewStreamDatagramFromBytes(b)
	if err != nil {
		return 0, 0, false, false
	}
	return envelope.AddressFromBytes(sd.LSF.Dst[:]), envelope.AddressFromBytes(sd.LSF.Src[:]), relayed(sd.LSF), true
}

// controlDatagram builds a 10-byte control datagram (ACKN, NACK, PING,
// PONG, DISC) carrying a callsign.
func controlDatagram(magic string, callsign envelope.Address) []byte {
	out := make([]byte, 10)
	copy(out, magic)
	c := callsign.Bytes()
	copy(out[4:10], c[:])
	return out
}
