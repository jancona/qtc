package envelope

import "fmt"

// Ack is the ACK view of an Envelope (client spec §4.1): a hop
// acknowledgement of one or more messages. Obtain it from Envelope.Ack.
type Ack struct{ e *Envelope }

const ackOffCount = 4

// Envelope returns the underlying envelope.
func (a Ack) Envelope() *Envelope { return a.e }

// IDs are the acknowledged message IDs.
func (a Ack) IDs() []ID {
	n := int(a.e.raw[ackOffCount])
	out := make([]ID, n)
	for i := range out {
		off := AckHeaderLen + i*IDLen
		out[i] = IDFromBytes(a.e.raw[off : off+IDLen])
	}
	return out
}

// MaxAckIDs is how many IDs fit in one ACK.
const MaxAckIDs = (MaxPayload - AckHeaderLen) / IDLen

// BuildAck produces an ACK for the given message IDs.
func BuildAck(ids []ID) (*Envelope, error) {
	if len(ids) == 0 || len(ids) > MaxAckIDs {
		return nil, fmt.Errorf("envelope: ACK of %d IDs", len(ids))
	}
	raw := make([]byte, AckHeaderLen, AckHeaderLen+len(ids)*IDLen)
	raw[offType] = byte(TypeQTC)
	raw[offKind] = byte(KindACK)
	raw[offVersion] = Version0
	raw[ackOffCount] = byte(len(ids))
	for _, id := range ids {
		raw = append(raw, id[:]...)
	}
	return Parse(raw)
}
