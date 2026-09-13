package envelope

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
)

// Msg is the MSG view of an Envelope (§4). Obtain it from Envelope.Msg.
type Msg struct{ e *Envelope }

// Envelope returns the underlying envelope.
func (m Msg) Envelope() *Envelope { return m.e }

// TTL is the time to live in minutes; see TTLLiveOnly and TTLDefault.
func (m Msg) TTL() uint16 { return binary.BigEndian.Uint16(m.e.raw[19:21]) }

// Nonce is the sender-chosen 16-bit value that distinguishes otherwise
// identical messages.
func (m Msg) Nonce() uint16 { return binary.BigEndian.Uint16(m.e.raw[21:23]) }

// Body is the message text. It is not checked for valid UTF-8; a
// store-and-forward node passes bytes through unchanged.
func (m Msg) Body() string { return string(m.e.raw[MsgHeaderLen:m.e.sigStart()]) }

// RcptReq reports whether the sender requested a DELIVERED receipt. It is
// ignored for room messages (§4.7); that is the caller's rule to apply.
func (m Msg) RcptReq() bool { return m.e.Flags()&FlagRcptReq != 0 }

// Expiry is Timestamp + TTL×60 as Unix seconds (§4.4). It reports false when
// the envelope alone does not determine expiry: the timestamp is 0 (unknown)
// or the TTL is TTLDefault, in which case the node decides.
func (m Msg) Expiry() (uint32, bool) {
	ts, ttl := m.e.Timestamp(), m.TTL()
	if ts == 0 || ttl == TTLDefault {
		return 0, false
	}
	exp := uint64(ts) + uint64(ttl)*60
	if exp > math.MaxUint32 {
		return 0, false
	}
	return uint32(exp), true
}

// BuildMsg produces an unsigned MSG. flags may carry RCPT_REQ and reserved
// bits but not SIGNED; use Envelope.Sign for that. body must be at most
// MaxBodyUnsigned bytes.
func BuildMsg(src, dst Address, timestamp uint32, ttl, nonce uint16, flags byte, body string) (*Envelope, error) {
	if flags&FlagSigned != 0 {
		return nil, fmt.Errorf("envelope: BuildMsg cannot set SIGNED; use Sign")
	}
	if len(body) > MaxBodyUnsigned {
		return nil, fmt.Errorf("envelope: body of %d bytes exceeds %d", len(body), MaxBodyUnsigned)
	}
	raw := make([]byte, MsgHeaderLen, MsgHeaderLen+len(body))
	raw[0] = byte(TypeMSG)
	raw[1] = Version0
	raw[2] = flags
	src.put(raw[3:9])
	dst.put(raw[9:15])
	binary.BigEndian.PutUint32(raw[15:19], timestamp)
	binary.BigEndian.PutUint16(raw[19:21], ttl)
	binary.BigEndian.PutUint16(raw[21:23], nonce)
	raw = append(raw, body...)
	return Parse(raw)
}

// NewNonce returns a random nonce from crypto/rand.
func NewNonce() (uint16, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("envelope: nonce: %w", err)
	}
	return binary.BigEndian.Uint16(b[:]), nil
}
