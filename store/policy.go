package store

import (
	"fmt"

	"github.com/jancona/pigeon/envelope"
)

// Policy holds the inbox's retention settings (node protocol §5 and §11,
// envelope §4.4).
type Policy struct {
	// DefaultTTL in minutes for a MSG with TTL 0xFFFF and for every RCPT.
	// 0 means 7 days.
	DefaultTTL uint16
	// MaxTTL caps a MSG's TTL in minutes; 0 means no cap.
	MaxTTL uint16
	// RoomRetention in seconds for ROOM envelopes; 0 means 30 days.
	RoomRetention uint32
	// FutureTolerance in seconds. A MSG timestamp further in the future than
	// this is treated as now for expiry. 0 means one hour.
	FutureTolerance uint32
}

// Defaults.
const (
	DefaultTTLMinutes      uint16 = 7 * 24 * 60
	DefaultRoomRetention   uint32 = 30 * 24 * 3600
	DefaultFutureTolerance uint32 = 3600
)

func (p Policy) defaultTTL() uint16 {
	if p.DefaultTTL == 0 {
		return DefaultTTLMinutes
	}
	return p.DefaultTTL
}

// Expiry decides whether an envelope may be stored at time now and, if so,
// when it expires. A refusal is a *PutError with the ID left zero.
func (p Policy) Expiry(e *envelope.Envelope, now uint32) (uint32, error) {
	switch e.Type() {
	case envelope.TypeMSG:
		m, _ := e.Msg()
		ttl := m.TTL()
		switch {
		case ttl == envelope.TTLLiveOnly:
			return 0, &PutError{Code: CodeRefused, Reason: "live only (TTL 0)"}
		case ttl == envelope.TTLDefault:
			ttl = p.defaultTTL()
		}
		if p.MaxTTL > 0 && ttl > p.MaxTTL {
			ttl = p.MaxTTL
		}
		ts := e.Timestamp()
		tol := p.FutureTolerance
		if tol == 0 {
			tol = DefaultFutureTolerance
		}
		if ts == 0 || ts > now+tol {
			ts = now
		}
		exp := ts + uint32(ttl)*60
		if exp <= now {
			return 0, &PutError{Code: CodeExpired, Reason: fmt.Sprintf("expired at %d", exp)}
		}
		return exp, nil
	case envelope.TypeRCPT:
		return now + uint32(p.defaultTTL())*60, nil
	case envelope.TypeROOM:
		r := p.RoomRetention
		if r == 0 {
			r = DefaultRoomRetention
		}
		return now + r, nil
	}
	return 0, &PutError{Code: CodeInvalid, Reason: fmt.Sprintf("packet type %s", e.Type())}
}
