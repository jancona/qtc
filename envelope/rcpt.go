package envelope

import (
	"encoding/binary"
	"fmt"
)

// Rcpt is the RCPT view of an Envelope (§5). Obtain it from Envelope.Rcpt.
type Rcpt struct{ e *Envelope }

// Envelope returns the underlying envelope.
func (r Rcpt) Envelope() *Envelope { return r.e }

// MessageID is the ID of the message the receipt is about.
func (r Rcpt) MessageID() ID { return IDFromBytes(r.e.raw[15:23]) }

// Status is the receipt status (§5.2).
func (r Rcpt) Status() Status { return Status(r.e.raw[23]) }

// LastHeard is when the issuing node last heard the recipient; 0 = never or
// not applicable.
func (r Rcpt) LastHeard() uint32 { return binary.BigEndian.Uint32(r.e.raw[28:32]) }

// Note is the optional text, typically a rejection reason.
func (r Rcpt) Note() string { return string(r.e.raw[RcptHeaderLen:r.e.sigStart()]) }

// BuildRcpt produces an unsigned RCPT. src is whoever observed the status
// and dst is the original message's source.
func BuildRcpt(src, dst Address, id ID, status Status, timestamp, lastHeard uint32, note string) (*Envelope, error) {
	if len(note) > MaxPayload-RcptHeaderLen {
		return nil, fmt.Errorf("envelope: note of %d bytes exceeds %d", len(note), MaxPayload-RcptHeaderLen)
	}
	raw := make([]byte, RcptHeaderLen, RcptHeaderLen+len(note))
	raw[0] = byte(TypeRCPT)
	raw[1] = Version0
	raw[2] = 0
	src.put(raw[3:9])
	dst.put(raw[9:15])
	copy(raw[15:23], id[:])
	raw[23] = byte(status)
	binary.BigEndian.PutUint32(raw[24:28], timestamp)
	binary.BigEndian.PutUint32(raw[28:32], lastHeard)
	raw = append(raw, note...)
	return Parse(raw)
}
