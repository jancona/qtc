package qtcd

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jancona/qtc/envelope"
)

func bodyOf(e *envelope.Envelope) string {
	m, _ := e.Msg()
	return m.Body()
}

// TestReplayCap: a replay sends the ReplayLimit most recent messages oldest
// first, after one notice for the rest, which never come back.
func TestReplayCap(t *testing.T) {
	var got deliveries
	r := startStation(t, Config{Callsign: "N1ADJ  Z"}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	r.presence.heard(ht, ViaLocal, now)

	var msgs []*envelope.Envelope
	for i := 14; i >= 0; i-- { // out of order on purpose
		msgs = append(msgs, mustMsg(t, w1aw, ht, now-1000+uint32(i), 60, uint16(i), 0, fmt.Sprintf("m%02d", i)))
	}
	r.replay(ht.Base(), msgs, now)

	var bodies []string
	for _, d := range got.list {
		bodies = append(bodies, bodyOf(d.env))
	}
	want := []string{"5 older messages not sent", "m05", "m06", "m07", "m08", "m09", "m10", "m11", "m12", "m13", "m14"}
	if fmt.Sprint(bodies) != fmt.Sprint(want) {
		t.Fatalf("replay sent %q\nwant %q", bodies, want)
	}
	if got.list[0].env.Source() != r.Callsign() {
		t.Errorf("notice from %s, want the node", got.list[0].env.Source())
	}
	for _, e := range msgs {
		if !r.delivered.has(deliveryKey{id: e.StoreID(), device: ht}) {
			t.Errorf("%s neither delivered nor recorded as skipped", bodyOf(e))
		}
	}

	r.replay(ht.Base(), msgs, now)
	if n := len(got.list); n != len(want) {
		t.Errorf("second replay sent %d more", n-len(want))
	}
}

// TestHeldUntilHeard: messages for a device out of reach wait in the
// mailbox and are replayed, capped, when the device is heard again.
func TestHeldUntilHeard(t *testing.T) {
	var got deliveries
	// A lone mailbox station: its own peer ID seeds its records.
	key := filepath.Join(t.TempDir(), "node.key")
	probe, err := New(Config{Callsign: "N1ADJ  Z", KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	id, err := probe.PeerID()
	if err != nil {
		t.Fatal(err)
	}
	r := startStation(t, Config{Callsign: "N1ADJ  Z", KeyFile: key, Caps: CapMailbox, MailboxMembers: []string{id.String()}}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	if err := r.Heard(ht, ViaLocal, now-2*3600); err != nil { // last heard two hours ago
		t.Fatal(err)
	}
	eventually(t, "homing", 5*time.Second, func() bool { return r.homes(ht) })

	for i := 0; i < 12; i++ {
		if _, err := r.server.PutLocal(ht, mustMsg(t, w1aw, ht, now-100+uint32(i), 60, uint16(i), 0, fmt.Sprintf("m%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if n := got.count(func(delivery) bool { return true }); n != 0 {
		t.Fatalf("%d messages sent to a device out of reach", n)
	}

	if err := r.Heard(ht, ViaLocal, now); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replay", 10*time.Second, func() bool { return got.count(func(delivery) bool { return true }) == 11 })
	got.mu.Lock()
	first, last := bodyOf(got.list[0].env), bodyOf(got.list[10].env)
	got.mu.Unlock()
	if first != "2 older messages not sent" || last != "m11" {
		t.Errorf("replay began %q and ended %q", first, last)
	}

	// Heard again while in reach, with nothing held: no second replay.
	if err := r.Heard(ht, ViaLocal, now+10); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := got.count(func(delivery) bool { return true }); n != 11 {
		t.Errorf("%d deliveries after hearing an in-reach device again, want 11", n)
	}
}

// TestHandoffFailureHolds: a device heard through the M17_inet face whose
// link is down does not get the message marked delivered; it is held.
func TestHandoffFailureHolds(t *testing.T) {
	var got deliveries
	r := startStation(t, Config{Callsign: "N1ADJ  Z", Inet: &InetConfig{
		Listen:  "127.0.0.1:0",
		Modules: map[byte]ModuleConfig{'A': {Reflector: "127.0.0.1:9", Module: 'C', Mode: ModeQTC}},
	}}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	r.presence.heard(ht, ViaRF, now) // heard over RF, but no gateway session now
	e := mustMsg(t, w1aw, ht, now, 60, 1, 0, "hello")

	r.deliverLocal(e)
	if r.delivered.has(deliveryKey{id: e.StoreID(), device: ht}) {
		t.Error("marked delivered with no link to hand it to")
	}
	r.mu.Lock()
	held := r.held[ht]
	r.mu.Unlock()
	if !held {
		t.Error("not held")
	}
	if n := got.count(func(delivery) bool { return true }); n != 0 {
		t.Errorf("%d deliveries recorded", n)
	}
}

// TestReplaySameSecondKeepsOrder: messages with one timestamp are replayed,
// and capped, in the order the mailbox gave them, not by ID.
func TestReplaySameSecondKeepsOrder(t *testing.T) {
	var got deliveries
	r := startStation(t, Config{Callsign: "N1ADJ  Z"}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	r.presence.heard(ht, ViaLocal, now)
	var msgs []*envelope.Envelope
	for i := 1; i <= 12; i++ {
		msgs = append(msgs, mustMsg(t, w1aw, ht, now-5, 60, uint16(1000-i), 0, fmt.Sprintf("test %d", i)))
	}
	r.replay(ht.Base(), msgs, now)
	var bodies []string
	for _, d := range got.list {
		bodies = append(bodies, bodyOf(d.env))
	}
	want := []string{"2 older messages not sent", "test 3", "test 4", "test 5", "test 6", "test 7", "test 8", "test 9", "test 10", "test 11", "test 12"}
	if fmt.Sprint(bodies) != fmt.Sprint(want) {
		t.Errorf("replay sent %q\nwant %q", bodies, want)
	}
}

// TestRelinkedReplaysHeld: messages held while a radio's gateway link was
// down are replayed when the link comes back, without the radio sending.
func TestRelinkedReplaysHeld(t *testing.T) {
	var got deliveries
	key := filepath.Join(t.TempDir(), "node.key")
	probe, err := New(Config{Callsign: "N1ADJ  Z", KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	id, err := probe.PeerID()
	if err != nil {
		t.Fatal(err)
	}
	r := startStation(t, Config{Callsign: "N1ADJ  Z", KeyFile: key, Caps: CapMailbox, MailboxMembers: []string{id.String()}}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	if err := r.Heard(ht, ViaLocal, now-2*3600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "homing", 5*time.Second, func() bool { return r.homes(ht) })
	for i := 0; i < 3; i++ {
		if _, err := r.server.PutLocal(ht, mustMsg(t, w1aw, ht, now-10+uint32(i), 60, uint16(i), 0, fmt.Sprintf("m%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "held", 2*time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.held[ht]
	})
	r.presence.heard(ht, ViaLocal, now) // in reach again, but not through Heard
	r.Relinked([]envelope.Address{ht})
	eventually(t, "replay on relink", 10*time.Second, func() bool { return got.count(func(delivery) bool { return true }) == 3 })
}

// TestDeliveryRecordsStopReplayElsewhere: after a replay, the mailbox holds
// a record for every message transmitted or skipped, so another node (here,
// the same station with an empty delivered-once table) replays nothing.
func TestDeliveryRecordsStopReplayElsewhere(t *testing.T) {
	var got deliveries
	key := filepath.Join(t.TempDir(), "node.key")
	probe, err := New(Config{Callsign: "N1ADJ  Z", KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	id, err := probe.PeerID()
	if err != nil {
		t.Fatal(err)
	}
	r := startStation(t, Config{Callsign: "N1ADJ  Z", KeyFile: key, Caps: CapMailbox, MailboxMembers: []string{id.String()}}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	if err := r.Heard(ht, ViaLocal, now-2*3600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "homing", 5*time.Second, func() bool { return r.homes(ht) })
	for i := 0; i < 12; i++ {
		if _, err := r.server.PutLocal(ht, mustMsg(t, w1aw, ht, now-100+uint32(i), 60, uint16(i), 0, fmt.Sprintf("m%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Stored messages reach delivery asynchronously; wait until they are held.
	time.Sleep(500 * time.Millisecond)
	if n := got.count(func(delivery) bool { return true }); n != 0 {
		t.Fatalf("%d messages sent to a device out of reach", n)
	}
	if err := r.Heard(ht, ViaLocal, now); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replay", 10*time.Second, func() bool { return got.count(func(delivery) bool { return true }) == 11 })

	// The mailbox holds 10 transmitted and 2 skipped records, addressed to
	// the recipient; none was delivered to the device.
	records := func() (tx, skip int) {
		recs, _, _, _ := r.mem.Query(ht.Base(), 0, 100, []envelope.PacketType{envelope.TypeRCPT})
		for _, rec := range recs {
			rc, _ := rec.Env.Rcpt()
			if rec.Env.Destination() != ht.Base() {
				continue
			}
			switch rc.Note() {
			case recordTransmitted:
				tx++
			case recordSkipped:
				skip++
			}
		}
		return
	}
	eventually(t, "records stored", 5*time.Second, func() bool { tx, skip := records(); return tx == 10 && skip == 2 })
	if n := got.count(func(d delivery) bool { return d.env.Type() == envelope.TypeRCPT }); n != 0 {
		t.Errorf("%d delivery records were delivered to the device", n)
	}

	// Another node knows nothing locally, but the mailbox says it is done.
	r.delivered = newDeliveredTable()
	r.mu.Lock()
	h := r.homed[ht.Base()]
	r.mu.Unlock()
	r.sweepSince(h, 0)
	time.Sleep(300 * time.Millisecond)
	if n := got.count(func(delivery) bool { return true }); n != 11 {
		t.Errorf("%d deliveries after a sweep with no local history, want still 11", n)
	}
}

// TestReplayOnlyToSweptCallsign: sweeping a sender's mailbox, which holds
// copies of what it sent, must not replay those messages to their
// recipients; their own mailbox's sweep does that, with their records.
func TestReplayOnlyToSweptCallsign(t *testing.T) {
	var got deliveries
	r := startStation(t, Config{Callsign: "N1ADJ  Z"}, &got)
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	now := unixNow()
	r.presence.heard(ht, ViaLocal, now)
	r.presence.heard(w1aw, ViaLocal, now)
	msgs := []*envelope.Envelope{mustMsg(t, w1aw, ht, now-5, 60, 1, 0, "sent by W1AW")}

	r.replay(w1aw.Base(), msgs, now) // W1AW's mailbox sweep
	if n := got.count(func(delivery) bool { return true }); n != 0 {
		t.Fatalf("sweeping the sender's mailbox delivered %d to the recipient", n)
	}
	r.replay(ht.Base(), msgs, now) // the recipient's own sweep
	if n := got.count(func(delivery) bool { return true }); n != 1 {
		t.Errorf("recipient's sweep delivered %d, want 1", n)
	}
}
