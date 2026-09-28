package envelope

import (
	"bytes"
	"fmt"
)

// FromSMS translates a legacy SMS payload (type byte 0x05 followed by NUL
// terminated text) into an unsigned MSG (§6). src and dst come from the
// received packet's LSF, timestamp is the node's current time, and ttl is
// the node's default. The text is cut at the first NUL and trimmed of ASCII
// whitespace.
func FromSMS(sms []byte, src, dst Address, timestamp uint32, ttl, nonce uint16) (*Envelope, error) {
	if len(sms) == 0 || PacketType(sms[0]) != TypeSMS {
		return nil, fmt.Errorf("%w: not an SMS payload", ErrInvalid)
	}
	text := sms[1:]
	if i := bytes.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	text = bytes.Trim(text, " \t\r\n")
	return BuildMsg(src, dst, timestamp, ttl, nonce, 0, string(text))
}

// ToSMS translates a MSG into a legacy SMS payload: the type byte, the body,
// and a terminating NUL. The signature, if any, is dropped. The LSF
// addresses for the packet are the envelope's Source and Destination.
func ToSMS(e *Envelope) ([]byte, error) {
	m, ok := e.Msg()
	if !ok {
		return nil, fmt.Errorf("envelope: cannot convert %s to SMS", e.Kind())
	}
	body := m.Body()
	out := make([]byte, 0, len(body)+2)
	out = append(out, byte(TypeSMS))
	out = append(out, body...)
	return append(out, 0), nil
}
