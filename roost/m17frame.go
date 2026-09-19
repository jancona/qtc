package roost

import (
	"encoding/binary"
	"fmt"

	"github.com/jancona/pigeon/envelope"
)

// Minimal M17_inet framing for the client face: just enough to relay
// datagrams, read source and destination addresses, and build SMS packets.
// Kept local rather than importing github.com/jancona/m17, whose root
// package links modem, audio, serial, and ZeroMQ dependencies.

// M17_inet datagram magics.
const (
	magicCONN = "CONN"
	magicLSTN = "LSTN"
	magicACKN = "ACKN"
	magicNACK = "NACK"
	magicPING = "PING"
	magicPONG = "PONG"
	magicDISC = "DISC"
	magicM17S = "M17 " // stream frame
	magicM17P = "M17P" // packet
)

const (
	lsfLen        = 30 // DST SRC TYPE META CRC
	lsdLen        = 28 // LSF without CRC, as carried in stream frames
	streamDgLen   = 4 + 2 + lsdLen + 2 + 16 + 2
	lsfTypePacket = 0x0002 // packet mode, data type "data"
)

// m17CRC is the M17 CRC-16 (polynomial 0x5935, init 0xFFFF, no reflection).
func m17CRC(b []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, x := range b {
		crc ^= uint16(x) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x5935
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// buildLSF returns a 30-byte packet-mode LSF with empty META.
func buildLSF(dst, src envelope.Address) []byte {
	lsf := make([]byte, lsfLen)
	d, s := dst.Bytes(), src.Bytes()
	copy(lsf[0:6], d[:])
	copy(lsf[6:12], s[:])
	binary.BigEndian.PutUint16(lsf[12:14], lsfTypePacket)
	binary.BigEndian.PutUint16(lsf[28:30], m17CRC(lsf[:28]))
	return lsf
}

// buildPacketDatagram frames a packet payload (type byte and contents,
// without CRC) as an M17_inet "M17P" datagram.
func buildPacketDatagram(dst, src envelope.Address, payload []byte) []byte {
	out := make([]byte, 0, 4+lsfLen+len(payload)+2)
	out = append(out, magicM17P...)
	out = append(out, buildLSF(dst, src)...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint16(out, m17CRC(payload))
}

// packetFrame is a parsed "M17P" datagram.
type packetFrame struct {
	dst, src envelope.Address
	typ      envelope.PacketType
	payload  []byte // type byte through the end of the contents, CRC stripped
}

// parsePacketDatagram parses an "M17P" datagram. The packet type is read
// as a single byte; Pigeon's and SMS's types are all below 0x80.
func parsePacketDatagram(b []byte) (packetFrame, error) {
	var f packetFrame
	if len(b) < 4+lsfLen+1+2 {
		return f, fmt.Errorf("packet datagram of %d bytes too short", len(b))
	}
	lsf := b[4 : 4+lsfLen]
	if m17CRC(lsf) != 0 {
		return f, fmt.Errorf("packet LSF CRC mismatch")
	}
	body := b[4+lsfLen:]
	if m17CRC(body) != 0 {
		return f, fmt.Errorf("packet payload CRC mismatch")
	}
	f.dst = envelope.AddressFromBytes(lsf[0:6])
	f.src = envelope.AddressFromBytes(lsf[6:12])
	f.payload = body[:len(body)-2]
	f.typ = envelope.PacketType(f.payload[0])
	return f, nil
}

// streamAddrs reads the destination and source from a stream frame.
func streamAddrs(b []byte) (dst, src envelope.Address, ok bool) {
	if len(b) != streamDgLen {
		return 0, 0, false
	}
	return envelope.AddressFromBytes(b[6:12]), envelope.AddressFromBytes(b[12:18]), true
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
