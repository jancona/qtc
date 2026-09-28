package client

import (
	"sync"
	"testing"
	"time"

	"github.com/jancona/qtc/envelope"
)

type sent struct {
	dst envelope.Address
	e   *envelope.Envelope
}

type recorder struct {
	mu     sync.Mutex
	sent   []sent
	events []Event
}

func (r *recorder) send(dst envelope.Address, e *envelope.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sent{dst, e})
}

func (r *recorder) event(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

// take returns and clears what was sent.
func (r *recorder) take() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sent
	r.sent = nil
	return out
}

func (r *recorder) messages() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev.Kind == EventMessage {
			n++
		}
	}
	return n
}

// fetches keeps only the SYNC FETCH packets: an unanswered sync REQUEST
// is repeated alongside them.
func fetches(x []sent) []sent {
	var out []sent
	for _, p := range x {
		if y, ok := p.e.Sync(); ok && y.Op() == envelope.SyncFetch {
			out = append(out, p)
		}
	}
	return out
}

func addr(t *testing.T, s string) envelope.Address {
	t.Helper()
	a, err := envelope.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func msg(t *testing.T, src, dst envelope.Address, nonce uint16, body string) *envelope.Envelope {
	t.Helper()
	e, err := envelope.BuildMsg(src, dst, uint32(time.Now().Unix()), 60, nonce, 0, body)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newRF(t *testing.T) (*Session, *recorder) {
	t.Helper()
	rec := &recorder{}
	s := New(Config{Me: addr(t, "N1ADJ 7"), RF: true, AckTimeout: 50 * time.Millisecond, AckJitter: 1, FetchDelay: 1, PageQuiet: 100 * time.Millisecond, Send: rec.send, Event: rec.event})
	return s, rec
}

// TestRFFiltering: on the air, a radio takes MSGs for its callsign, its
// device, and rooms it is in, and nothing else; control packets for other
// radios are ignored.
func TestRFFiltering(t *testing.T) {
	s, rec := newRF(t)
	node, w1aw := addr(t, "N1ADJ   Q"), addr(t, "W1AW")
	maine, _ := envelope.RoomAddress("MAINE")
	net, _ := envelope.RoomAddress("NET")
	s.Linked()
	rec.take() // the sync request

	// Before the room list is known, room messages are taken.
	s.Receive(maine, w1aw, msg(t, w1aw, maine, 1, "maine"))
	s.Receive(addr(t, "N1ADJ"), w1aw, msg(t, w1aw, addr(t, "N1ADJ"), 2, "to the callsign"))
	s.Receive(addr(t, "N1ADJ 7"), w1aw, msg(t, w1aw, addr(t, "N1ADJ 7"), 3, "to this radio"))
	s.Receive(addr(t, "N1ADJ 8"), w1aw, msg(t, w1aw, addr(t, "N1ADJ 8"), 4, "to another radio"))
	s.Receive(addr(t, "K1ABC"), w1aw, msg(t, w1aw, addr(t, "K1ABC"), 5, "to someone else"))
	if n := rec.messages(); n != 3 {
		t.Errorf("took %d messages, want 3", n)
	}
	// Direct ones get DELIVERED; a room message on the air outside a sync
	// page gets nothing.
	var dl int
	for _, x := range rec.take() {
		if rc, ok := x.e.Rcpt(); ok && rc.Status() == envelope.StatusDelivered {
			dl++
		} else {
			t.Errorf("sent %s", x.e)
		}
	}
	if dl != 2 {
		t.Errorf("%d DELIVERED, want 2", dl)
	}

	// Once the room list is known, other rooms are not taken.
	s.RoomRequest(envelope.OpList, nil)
	if x := rec.take(); len(x) != 1 || x[0].dst != envelope.Broadcast {
		t.Fatalf("LIST sent %+v; the node is not known yet, so broadcast", x)
	}
	reply, _ := envelope.BuildRoom(envelope.OpOK, 1, []envelope.Address{maine}, "")
	s.Receive(addr(t, "N1ADJ 8"), node, reply) // another radio's reply: ignored
	if s.Node() != 0 {
		t.Error("learned the node from another radio's packet")
	}
	s.Receive(addr(t, "N1ADJ 7"), node, reply)
	if s.Node() != node {
		t.Errorf("node = %s", s.Node())
	}
	s.Receive(net, w1aw, msg(t, w1aw, net, 6, "net"))
	s.Receive(maine, w1aw, msg(t, w1aw, maine, 7, "maine again"))
	if n := rec.messages(); n != 4 {
		t.Errorf("took %d messages, want 4", n)
	}
}

// TestRFSummaryFetch: a summary listing a room message this radio missed
// brings a FETCH, unless the message turns up first; retries are limited.
func TestRFSummaryFetch(t *testing.T) {
	s, rec := newRF(t)
	node, w1aw := addr(t, "N1ADJ   Q"), addr(t, "W1AW")
	maine, _ := envelope.RoomAddress("MAINE")
	s.Linked()
	rec.take()
	had := msg(t, w1aw, maine, 1, "had")
	missed := msg(t, w1aw, maine, 2, "missed")
	late := msg(t, w1aw, maine, 3, "late")
	s.Receive(maine, w1aw, had)

	sum, _ := envelope.BuildSyncSummary([]envelope.SummaryGroup{{Room: maine, IDs: []envelope.ShortID{had.ID().Short(), missed.ID().Short(), late.ID().Short()}}})
	s.Receive(envelope.Broadcast, node, sum)
	s.Receive(maine, w1aw, late) // heard before the FETCH went out
	time.Sleep(5 * time.Millisecond)
	s.Tick(time.Now())
	x := fetches(rec.take())
	if len(x) != 1 || x[0].dst != node {
		t.Fatalf("sent %+v", x)
	}
	y, ok := x[0].e.Sync()
	if !ok || y.Op() != envelope.SyncFetch || len(y.ShortIDs()) != 1 || y.ShortIDs()[0] != missed.ID().Short() {
		t.Fatalf("FETCH %s", x[0].e)
	}
	// Still missing: FETCH again after the ack timeout, up to the retry
	// limit, then left to sync.
	n := 1
	for range 20 {
		time.Sleep(60 * time.Millisecond)
		s.Tick(time.Now())
		n += len(fetches(rec.take()))
	}
	if n != 1+s.cfg.AckRetries {
		t.Errorf("%d FETCHes, want %d", n, 1+s.cfg.AckRetries)
	}

	// Once it arrives, a later summary asks for nothing.
	s.Receive(maine, w1aw, missed)
	s.Receive(envelope.Broadcast, node, sum)
	s.Tick(time.Now().Add(time.Second))
	if x := fetches(rec.take()); len(x) != 0 {
		t.Errorf("sent %+v with nothing missing", x)
	}
}

// TestRFSyncPageAcksRooms: in a sync page a room message is acknowledged,
// since the page goes to this radio alone.
func TestRFSyncPageAcksRooms(t *testing.T) {
	s, rec := newRF(t)
	node, w1aw := addr(t, "N1ADJ   Q"), addr(t, "W1AW")
	maine, _ := envelope.RoomAddress("MAINE")
	s.Linked()
	rec.take()
	page, _ := envelope.BuildSync(envelope.SyncPage, 0, 10, 1, 1, 0)
	s.Receive(addr(t, "N1ADJ 7"), node, page)
	m := msg(t, w1aw, maine, 1, "backlog")
	s.Receive(maine, w1aw, m)
	x := rec.take()
	if len(x) != 1 {
		t.Fatalf("sent %+v", x)
	}
	if a, ok := x[0].e.Ack(); !ok || a.IDs()[0] != m.ID() || x[0].dst != node {
		t.Errorf("sent %s to %s", x[0].e, x[0].dst)
	}
	// The page is complete: the position is taken and the sync ends.
	time.Sleep(1100 * time.Millisecond)
	s.Tick(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sync.active || s.sync.cursor != 10 || s.sync.skip != 1 {
		t.Errorf("sync %+v", s.sync)
	}
}
