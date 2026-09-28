package envelope

import (
	"encoding/binary"
	"fmt"
)

// Sync is the SYNC view of an Envelope (client spec §5.1). Obtain it from
// Envelope.Sync. Like ROOM, SYNC is a control packet: its addresses come
// from the LSF and no signature is defined for it.
type Sync struct{ e *Envelope }

// SYNC offsets (client spec §5.1).
const (
	syncOffOp        = 4
	syncOffCursor    = 5
	syncOffSkip      = 9
	syncOffCount     = 11
	syncOffRemaining = 12
)

// Envelope returns the underlying envelope.
func (y Sync) Envelope() *Envelope { return y.e }

// Op is the operation.
func (y Sync) Op() SyncOp { return SyncOp(y.e.raw[syncOffOp]) }

// Cursor is a mailbox received-at time, echoed by the device (§5.2).
func (y Sync) Cursor() uint32 {
	return binary.BigEndian.Uint32(y.e.raw[syncOffCursor : syncOffCursor+4])
}

// Skip counts entries at exactly Cursor already sent.
func (y Sync) Skip() uint16 { return binary.BigEndian.Uint16(y.e.raw[syncOffSkip : syncOffSkip+2]) }

// Count is, for REQUEST, the page size wanted; for PAGE, how many packets
// follow; for FETCH, the number of IDs; for SUMMARY, the number of groups.
func (y Sync) Count() int { return int(y.e.raw[syncOffCount]) }

// Remaining is, for PAGE, how many are left after this page, and for NOTIFY,
// how many are waiting. 0xFFFF means that many or more.
func (y Sync) Remaining() uint16 {
	return binary.BigEndian.Uint16(y.e.raw[syncOffRemaining : syncOffRemaining+2])
}

// All and Sent report the REQUEST flags.
func (y Sync) All() bool  { return y.e.Flags()&SyncFlagAll != 0 }
func (y Sync) Sent() bool { return y.e.Flags()&SyncFlagSent != 0 }

// Note is the reason in a REFUSED.
func (y Sync) Note() string {
	if y.Op() != SyncRefused {
		return ""
	}
	return string(y.e.raw[SyncHeaderLen:])
}

// ShortIDs are the 4-byte message ID prefixes of a FETCH.
func (y Sync) ShortIDs() []ShortID {
	if y.Op() != SyncFetch {
		return nil
	}
	return shortIDs(y.e.raw[SyncHeaderLen:])
}

// SummaryGroup is one room's entries in a SUMMARY (client spec §6.1).
type SummaryGroup struct {
	Room Address
	IDs  []ShortID
}

// Groups are the entries of a SUMMARY.
func (y Sync) Groups() []SummaryGroup {
	if y.Op() != SyncSummary {
		return nil
	}
	tail := y.e.raw[SyncHeaderLen:]
	out := make([]SummaryGroup, 0, y.Count())
	for len(tail) > 0 {
		n := int(tail[AddressLen])
		end := AddressLen + 1 + n*ShortIDLen
		out = append(out, SummaryGroup{Room: AddressFromBytes(tail[:AddressLen]), IDs: shortIDs(tail[AddressLen+1 : end])})
		tail = tail[end:]
	}
	return out
}

// ShortID is the first 4 bytes of a message ID.
type ShortID [ShortIDLen]byte

// Short returns the ID's 4-byte prefix.
func (id ID) Short() ShortID {
	var s ShortID
	copy(s[:], id[:ShortIDLen])
	return s
}

func shortIDs(b []byte) []ShortID {
	out := make([]ShortID, len(b)/ShortIDLen)
	for i := range out {
		copy(out[i][:], b[i*ShortIDLen:])
	}
	return out
}

// checkSyncTail validates the Tail of a FETCH or SUMMARY against Count.
func checkSyncTail(op SyncOp, count int, tail []byte) error {
	switch op {
	case SyncFetch:
		if len(tail) != count*ShortIDLen {
			return fmt.Errorf("%w: FETCH count %d with %d bytes of IDs", ErrInvalid, count, len(tail))
		}
	case SyncSummary:
		for g := 0; g < count; g++ {
			if len(tail) < AddressLen+1 {
				return fmt.Errorf("%w: SUMMARY group %d truncated", ErrInvalid, g)
			}
			end := AddressLen + 1 + int(tail[AddressLen])*ShortIDLen
			if len(tail) < end {
				return fmt.Errorf("%w: SUMMARY group %d needs %d bytes, have %d", ErrInvalid, g, end, len(tail))
			}
			tail = tail[end:]
		}
		if len(tail) != 0 {
			return fmt.Errorf("%w: %d bytes after the last SUMMARY group", ErrInvalid, len(tail))
		}
	}
	return nil
}

// BuildSync produces a SYNC REQUEST, PAGE, or NOTIFY: every op whose Tail is
// empty. flags carries SyncFlagAll and SyncFlagSent on a REQUEST.
func BuildSync(op SyncOp, flags byte, cursor uint32, skip uint16, count int, remaining uint16) (*Envelope, error) {
	if count < 0 || count > 255 {
		return nil, fmt.Errorf("envelope: SYNC count %d is not 0-255", count)
	}
	return Parse(syncHeader(op, flags, cursor, skip, byte(count), remaining))
}

// BuildSyncRefused produces a SYNC REFUSED with a reason.
func BuildSyncRefused(note string) (*Envelope, error) {
	if len(note) > MaxPayload-SyncHeaderLen {
		note = note[:MaxPayload-SyncHeaderLen]
	}
	return Parse(append(syncHeader(SyncRefused, 0, 0, 0, 0, 0), note...))
}

// BuildSyncFetch produces a SYNC FETCH for the given short IDs.
func BuildSyncFetch(ids []ShortID) (*Envelope, error) {
	if len(ids) == 0 || len(ids) > 255 {
		return nil, fmt.Errorf("envelope: FETCH of %d IDs", len(ids))
	}
	raw := syncHeader(SyncFetch, 0, 0, 0, byte(len(ids)), 0)
	for _, id := range ids {
		raw = append(raw, id[:]...)
	}
	return Parse(raw)
}

// BuildSyncSummary produces a SYNC SUMMARY of the given groups.
func BuildSyncSummary(groups []SummaryGroup) (*Envelope, error) {
	if len(groups) == 0 || len(groups) > 255 {
		return nil, fmt.Errorf("envelope: SUMMARY of %d groups", len(groups))
	}
	raw := syncHeader(SyncSummary, 0, 0, 0, byte(len(groups)), 0)
	for _, g := range groups {
		if len(g.IDs) > 255 {
			return nil, fmt.Errorf("envelope: SUMMARY group of %d IDs", len(g.IDs))
		}
		var a [AddressLen]byte
		g.Room.put(a[:])
		raw = append(raw, a[:]...)
		raw = append(raw, byte(len(g.IDs)))
		for _, id := range g.IDs {
			raw = append(raw, id[:]...)
		}
	}
	return Parse(raw)
}

func syncHeader(op SyncOp, flags byte, cursor uint32, skip uint16, count byte, remaining uint16) []byte {
	raw := make([]byte, SyncHeaderLen)
	raw[offType] = byte(TypeQTC)
	raw[offKind] = byte(KindSYNC)
	raw[offVersion] = Version0
	raw[offFlags] = flags
	raw[syncOffOp] = byte(op)
	binary.BigEndian.PutUint32(raw[syncOffCursor:], cursor)
	binary.BigEndian.PutUint16(raw[syncOffSkip:], skip)
	raw[syncOffCount] = count
	binary.BigEndian.PutUint16(raw[syncOffRemaining:], remaining)
	return raw
}
