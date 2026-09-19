package store

import (
	"sort"
	"sync"

	"github.com/jancona/qtc/envelope"
)

// Record is a stored envelope.
type Record struct {
	Callsign   envelope.Address // base callsign or room the envelope is stored under
	Env        *envelope.Envelope
	ReceivedAt uint32 // time of the first PUT
	Expiry     uint32 // when to discard it
}

// ID is the storage key within a callsign.
func (r Record) ID() envelope.ID { return r.Env.StoreID() }

// Store is mailbox storage keyed by callsign and StoreID. Implementations must
// be safe for concurrent use.
type Store interface {
	// Put stores rec unless an envelope with the same callsign and ID is
	// already present, reporting whether it was stored. It returns ErrQuota
	// when a quota is exceeded.
	Put(rec Record) (stored bool, err error)
	// Query returns records for callsign with ReceivedAt >= since, oldest
	// first, filtered to the given packet types (all types if empty). It
	// returns at least limit records when that many exist, extended so that
	// every record sharing the last record's ReceivedAt is included. more
	// reports whether records after the returned page exist; next is the
	// last record's ReceivedAt, from which the caller queries again.
	Query(callsign envelope.Address, since uint32, limit int, types []envelope.PacketType) (recs []Record, next uint32, more bool, err error)
	// Expire discards records whose Expiry is at or before now and reports
	// how many were dropped.
	Expire(now uint32) int
}

// MemStore is an in-memory Store.
type MemStore struct {
	// MaxPerCallsign caps the records kept for one callsign; 0 is unlimited.
	// The cap is a placeholder for the quota policy the node protocol leaves
	// open.
	MaxPerCallsign int

	mu    sync.Mutex
	boxes map[envelope.Address]*box
}

type box struct {
	byID map[envelope.ID]Record
	recs []Record // sorted by ReceivedAt, then insertion order
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{boxes: map[envelope.Address]*box{}}
}

// Put implements Store.
func (m *MemStore) Put(rec Record) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.boxes[rec.Callsign]
	if b == nil {
		b = &box{byID: map[envelope.ID]Record{}}
		m.boxes[rec.Callsign] = b
	}
	id := rec.ID()
	if _, dup := b.byID[id]; dup {
		return false, nil
	}
	if m.MaxPerCallsign > 0 && len(b.recs) >= m.MaxPerCallsign {
		return false, ErrQuota
	}
	b.byID[id] = rec
	i := sort.Search(len(b.recs), func(i int) bool { return b.recs[i].ReceivedAt > rec.ReceivedAt })
	b.recs = append(b.recs, Record{})
	copy(b.recs[i+1:], b.recs[i:])
	b.recs[i] = rec
	return true, nil
}

// Query implements Store.
func (m *MemStore) Query(callsign envelope.Address, since uint32, limit int, types []envelope.PacketType) ([]Record, uint32, bool, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	want := func(t envelope.PacketType) bool {
		if len(types) == 0 {
			return true
		}
		for _, x := range types {
			if x == t {
				return true
			}
		}
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.boxes[callsign]
	if b == nil {
		return nil, 0, false, nil
	}
	var out []Record
	start := sort.Search(len(b.recs), func(i int) bool { return b.recs[i].ReceivedAt >= since })
	i := start
	for ; i < len(b.recs); i++ {
		r := b.recs[i]
		if !want(r.Env.Type()) {
			continue
		}
		if len(out) >= limit && r.ReceivedAt != out[len(out)-1].ReceivedAt {
			break
		}
		out = append(out, r)
	}
	more := false
	for ; i < len(b.recs); i++ {
		if want(b.recs[i].Env.Type()) {
			more = true
			break
		}
	}
	var next uint32
	if len(out) > 0 {
		next = out[len(out)-1].ReceivedAt
	}
	return out, next, more, nil
}

// Expire implements Store.
func (m *MemStore) Expire(now uint32) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for call, b := range m.boxes {
		kept := b.recs[:0]
		for _, r := range b.recs {
			if r.Expiry <= now {
				delete(b.byID, r.ID())
				n++
				continue
			}
			kept = append(kept, r)
		}
		b.recs = kept
		if len(b.recs) == 0 {
			delete(m.boxes, call)
		}
	}
	return n
}

// Len reports the number of stored records.
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.boxes {
		n += len(b.recs)
	}
	return n
}
