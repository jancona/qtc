package qtcd

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/qtc/envelope"
)

// fastNative shortens the native-client timers for a test.
func fastNative(t *testing.T) {
	t.Helper()
	oldTick, oldInet, oldRF, oldJitter, oldSpacing, oldRetries := nativeTick, ackTimeoutInet, ackTimeoutRF, ackJitterRF, ackSpacingRF, ackRetries
	oldIdle, oldRepeat, oldEvery, oldDedup := summaryIdle, summaryRepeat, summaryEvery, fetchDedup
	nativeTick, ackTimeoutInet, ackTimeoutRF, ackJitterRF, ackSpacingRF, ackRetries = 20*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond, 0, 0, 2
	summaryIdle, summaryRepeat, summaryEvery, fetchDedup = 300*time.Millisecond, 600*time.Millisecond, 3, 0
	t.Cleanup(func() {
		nativeTick, ackTimeoutInet, ackTimeoutRF, ackJitterRF, ackSpacingRF, ackRetries = oldTick, oldInet, oldRF, oldJitter, oldSpacing, oldRetries
		summaryIdle, summaryRepeat, summaryEvery, fetchDedup = oldIdle, oldRepeat, oldEvery, oldDedup
	})
}

func startNativeFace(t *testing.T, core *stubCore, gateways []*net.IPNet) *inetFace {
	t.Helper()
	face, err := newInetFace(core, InetConfig{Listen: "127.0.0.1:0", Gateways: gateways, Modules: map[byte]ModuleConfig{'A': {Mode: ModeQTC}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { face.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return face
}

// expectQTC waits for a QTC packet and parses it.
func (p *udpPeer) expectQTC(t *testing.T) (packetFrame, *envelope.Envelope) {
	t.Helper()
	b, _ := p.expect(t, magicM17P)
	pf, err := parsePacketDatagram(b)
	if err != nil {
		t.Fatal(err)
	}
	if pf.typ != envelope.TypeQTC {
		t.Fatalf("expected a QTC packet, got type %s", pf.typ)
	}
	e, err := envelope.Parse(pf.payload)
	if err != nil {
		t.Fatal(err)
	}
	return pf, e
}

func (c *stubCore) ackEvents() ([]ackEvent, []ackEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ackEvent(nil), c.acked...), append([]ackEvent(nil), c.lost...)
}

// TestNativeAckAndRetry: a native device's MSG is taken once and
// acknowledged, however often it is retried; a MSG to it is resent until it
// answers DELIVERED, and given up on (held) if it never does. An SMS makes
// the device legacy again.
func TestNativeAckAndRetry(t *testing.T) {
	fastNative(t)
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face := startNativeFace(t, core, []*net.IPNet{}) // loopback is an internet client
	node, dev, w1aw := core.node, mustAddr(t, "N1ADJ  N"), mustAddr(t, "W1AW")
	client := newUDPPeer(t)
	client.sendTo(t, face.Addr(), connDatagram(dev, 'A'))
	client.expect(t, magicACKN)

	out := mustMsg(t, dev, w1aw, 1, 60, 1, 0, "hello")
	for range 2 { // the second is a retry
		client.sendTo(t, face.Addr(), buildPacketDatagram(w1aw, dev, out.Bytes()))
		pf, e := client.expectQTC(t)
		a, ok := e.Ack()
		if !ok || len(a.IDs()) != 1 || a.IDs()[0] != out.ID() || pf.src != node || pf.dst != dev {
			t.Fatalf("answer %s from %s to %s", e, pf.src, pf.dst)
		}
	}
	if n := core.sentCount(); n != 1 {
		t.Errorf("MSG sent %d times", n)
	}
	if !face.isNative(dev) {
		t.Fatal("device not native after sending QTC")
	}

	// A mismatched source is refused.
	spoof := mustMsg(t, mustAddr(t, "AB1CD"), w1aw, 1, 60, 2, 0, "spoof")
	client.sendTo(t, face.Addr(), buildPacketDatagram(w1aw, dev, spoof.Bytes()))
	if _, e := client.expectQTC(t); e.Kind() != envelope.KindRCPT {
		t.Errorf("spoofed MSG answered with %s", e)
	} else if rc, _ := e.Rcpt(); rc.Status() != envelope.StatusRejected {
		t.Errorf("spoofed MSG answered with %s", e)
	}

	// Delivery to the device: resent until DELIVERED.
	in := mustMsg(t, w1aw, dev.Base(), 1, 60, 3, envelope.FlagRcptReq, "for you")
	if res := face.deliver(dev, in); res != deliverPending {
		t.Fatalf("deliver = %d, want pending", res)
	}
	for range 2 {
		pf, e := client.expectQTC(t)
		if e.ID() != in.ID() || pf.dst != dev { // the device, though sent to the base callsign
			t.Fatalf("got %s to %s", e, pf.dst)
		}
	}
	dl, _ := envelope.BuildRcpt(dev, w1aw, in.ID(), envelope.StatusDelivered, 2, 0, "")
	client.sendTo(t, face.Addr(), buildPacketDatagram(node, dev, dl.Bytes()))
	eventually(t, "DELIVERED reported", 2*time.Second, func() bool { a, _ := core.ackEvents(); return len(a) == 1 })
	acked, _ := core.ackEvents()
	if acked[0].device != dev || acked[0].msg.ID() != in.ID() || acked[0].rcpt == nil || !acked[0].fresh {
		t.Errorf("acked %+v", acked[0])
	}
	client.expectNone(t, magicM17P, 500*time.Millisecond)

	// Never acknowledged: resent ackRetries times, then lost.
	gone := mustMsg(t, w1aw, dev, 1, 60, 4, 0, "into the void")
	face.deliver(dev, gone)
	eventually(t, "given up", 3*time.Second, func() bool { _, l := core.ackEvents(); return len(l) == 1 })
	if _, lost := core.ackEvents(); lost[0].msg.ID() != gone.ID() || !lost[0].fresh {
		t.Errorf("lost %+v", lost[0])
	}

	// An SMS makes the device legacy: delivery is SMS again.
	client.sendTo(t, face.Addr(), smsDatagram(w1aw, dev, "old habits"))
	eventually(t, "legacy again", 2*time.Second, func() bool { return !face.isNative(dev) })
	if res := face.deliver(dev, mustMsg(t, w1aw, dev, 1, 60, 5, 0, "sms")); res != deliverSent {
		t.Errorf("legacy deliver = %d", res)
	}
}

// TestNativeSync: a SYNC REQUEST is answered with a PAGE and then the
// page's packets; its MSGs are resent until acknowledged, and one ACK can
// cover them all.
func TestNativeSync(t *testing.T) {
	fastNative(t)
	dev, w1aw := mustAddr(t, "N1ADJ  N"), mustAddr(t, "W1AW")
	m1 := mustMsg(t, w1aw, dev.Base(), 1, 60, 1, 0, "one")
	m2 := mustMsg(t, w1aw, dev.Base(), 1, 60, 2, 0, "two")
	rc, _ := envelope.BuildRcpt(mustAddr(t, "K1XYZ  R"), dev.Base(), envelope.ID{1}, envelope.StatusTransmitted, 3, 0, "")
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ"), syncPage: []*envelope.Envelope{m1, m2, rc}}
	face := startNativeFace(t, core, []*net.IPNet{})
	client := newUDPPeer(t)
	client.sendTo(t, face.Addr(), connDatagram(dev, 'A'))
	client.expect(t, magicACKN)

	req, _ := envelope.BuildSync(envelope.SyncRequest, envelope.SyncFlagAll, 99, 2, 0, 0)
	client.sendTo(t, face.Addr(), buildPacketDatagram(envelope.Broadcast, dev, req.Bytes()))
	_, e := client.expectQTC(t)
	y, ok := e.Sync()
	if !ok || y.Op() != envelope.SyncPage || y.Count() != 3 || y.Cursor() != 1234 || y.Remaining() != 0 {
		t.Fatalf("reply %s", e)
	}
	got := map[envelope.ID]bool{}
	var rcpts int
	for range 3 {
		_, e := client.expectQTC(t)
		switch e.Kind() {
		case envelope.KindMSG:
			got[e.ID()] = true
		case envelope.KindRCPT:
			rcpts++
		}
	}
	if !got[m1.ID()] || !got[m2.ID()] || rcpts != 1 {
		t.Fatalf("page carried %v and %d receipts", got, rcpts)
	}
	core.mu.Lock()
	if len(core.syncReqs) != 1 || core.syncReqs[0].Cursor() != 99 || core.syncReqs[0].Skip() != 2 || !core.syncReqs[0].All() {
		t.Errorf("sync request %+v", core.syncReqs)
	}
	core.mu.Unlock()

	ack, _ := envelope.BuildAck([]envelope.ID{m1.ID(), m2.ID()})
	client.sendTo(t, face.Addr(), buildPacketDatagram(core.node, dev, ack.Bytes()))
	eventually(t, "page acknowledged", 2*time.Second, func() bool { a, _ := core.ackEvents(); return len(a) == 2 })
	for _, a := range core.acked {
		if a.fresh || a.rcpt != nil {
			t.Errorf("page ack %+v", a)
		}
	}

	// A device that just synced is not notified.
	face.notify(dev, 5)
	client.expectNone(t, magicM17P, 300*time.Millisecond)
}

// TestRoomSummaries: on an RF gateway, a room MSG goes out once to the room
// address, then a SUMMARY after the burst (and once more if all stays
// quiet); a FETCH brings a retransmission. A burst of summaryEvery gets a
// summary straight away.
func TestRoomSummaries(t *testing.T) {
	fastNative(t)
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ"), subscribed: true}
	face := startNativeFace(t, core, nil) // loopback is a gateway
	n1, n2, w1aw, maine := mustAddr(t, "N1ADJ  N"), mustAddr(t, "N1ADJ  M"), mustAddr(t, "W1AW"), mustRoom(t, "MAINE")
	gw := newUDPPeer(t)
	gw.sendTo(t, face.Addr(), connDatagram(mustAddr(t, "N1ADJ  G"), 'A'))
	gw.expect(t, magicACKN)
	// Two native radios behind the gateway.
	for _, d := range []envelope.Address{n1, n2} {
		ack, _ := envelope.BuildAck([]envelope.ID{{0xFF}})
		gw.sendTo(t, face.Addr(), buildPacketDatagram(core.node, d, ack.Bytes()))
	}
	eventually(t, "radios native", 2*time.Second, func() bool { return face.isNative(n1) && face.isNative(n2) })

	m := mustMsg(t, w1aw, maine, 1, 60, 1, 0, "net tonight")
	if face.deliver(n1, m) != deliverSent || face.deliver(n2, m) != deliverSent {
		t.Fatal("room MSG not sent")
	}
	pf, e := gw.expectQTC(t)
	if e.ID() != m.ID() || pf.dst != maine {
		t.Fatalf("transmitted %s to %s", e, pf.dst)
	}
	// Once only, then the burst-end summary, then its repeat.
	for _, what := range []string{"summary", "repeat"} {
		pf, e = gw.expectQTC(t)
		y, ok := e.Sync()
		if !ok || y.Op() != envelope.SyncSummary || pf.dst != envelope.Broadcast {
			t.Fatalf("%s: got %s to %s", what, e, pf.dst)
		}
		g := y.Groups()
		if len(g) != 1 || g[0].Room != maine || len(g[0].IDs) != 1 || g[0].IDs[0] != m.ID().Short() {
			t.Errorf("%s groups %+v", what, g)
		}
	}
	gw.expectNone(t, magicM17P, 800*time.Millisecond)

	fetch, _ := envelope.BuildSyncFetch([]envelope.ShortID{m.ID().Short()})
	gw.sendTo(t, face.Addr(), buildPacketDatagram(core.node, n1, fetch.Bytes()))
	if pf, e := gw.expectQTC(t); e.ID() != m.ID() || pf.dst != maine {
		t.Errorf("FETCH brought %s to %s", e, pf.dst)
	}

	// summaryEvery (3) messages: summary at once, before the idle timer.
	var burst []*envelope.Envelope
	for i := range 3 {
		b := mustMsg(t, w1aw, maine, 1, 60, uint16(10+i), 0, "burst")
		burst = append(burst, b)
		face.deliver(n1, b)
	}
	start := time.Now()
	for range 3 {
		gw.expectQTC(t)
	}
	_, e = gw.expectQTC(t)
	y, _ := e.Sync()
	if y.Op() != envelope.SyncSummary || time.Since(start) >= summaryIdle {
		t.Fatalf("after %v: %s", time.Since(start), e)
	}
	var ids []byte
	for _, g := range y.Groups() {
		for _, id := range g.IDs {
			ids = append(ids, id[:]...)
		}
	}
	for _, b := range append([]*envelope.Envelope{m}, burst...) {
		s := b.ID().Short()
		if !bytes.Contains(ids, s[:]) {
			t.Errorf("summary lacks %s", b.ID())
		}
	}
}

// loneMailboxStation starts a station that is the only member of every
// mailbox, so its own store is the whole mailbox.
func loneMailboxStation(t *testing.T, got *deliveries) *Station {
	t.Helper()
	key := t.TempDir() + "/node.key"
	probe, err := New(Config{Callsign: "N1ADJ  Z", KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	id, err := probe.PeerID()
	if err != nil {
		t.Fatal(err)
	}
	return startStation(t, Config{Callsign: "N1ADJ  Z", KeyFile: key, Caps: CapMailbox, MailboxMembers: []string{id.String()}}, got)
}

func syncReq(t *testing.T, flags byte, cursor uint32, skip uint16) envelope.Sync {
	t.Helper()
	e, err := envelope.BuildSync(envelope.SyncRequest, flags, cursor, skip, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := e.Sync()
	return y
}

// TestInetSyncPaging: sync pages through a callsign's mailbox in received
// order, a few at a time, even when everything arrived in one second. It
// leaves out other devices' messages, delivery records, and what the device
// already acknowledged, unless asked for ALL; SENT adds the callsign's own
// messages.
func TestInetSyncPaging(t *testing.T) {
	var got deliveries
	r := loneMailboxStation(t, &got)
	dev, other, w1aw := mustAddr(t, "N1ADJ  N"), mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	base := dev.Base()
	now := unixNow()
	var want []*envelope.Envelope
	for i := range 7 {
		e := mustMsg(t, w1aw, base, now, 60, uint16(i), 0, "m")
		want = append(want, e)
		if _, err := r.server.PutLocal(base, e); err != nil {
			t.Fatal(err)
		}
	}
	toDev := mustMsg(t, w1aw, dev, now, 60, 100, 0, "just this device")
	want = append(want, toDev)
	sent := mustMsg(t, dev, w1aw, now, 60, 101, 0, "sent by the callsign")
	record, _ := envelope.BuildRcpt(r.callsign, base, want[0].ID(), envelope.StatusTransmitted, now, 0, recordTransmitted)
	for _, e := range []*envelope.Envelope{toDev, mustMsg(t, w1aw, other, now, 60, 102, 0, "another device"), sent, record} {
		if _, err := r.server.PutLocal(base, e); err != nil {
			t.Fatal(err)
		}
	}

	// Pages of 3. The device acknowledges each page before asking again.
	var cursor uint32
	var skip uint16
	var seen []envelope.ID
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("sync never ended")
		}
		page, c, s, remaining, err := r.inetSync(dev, syncReq(t, 0, cursor, skip), 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			seen = append(seen, e.ID())
			r.inetAcked(dev, e, nil, false)
		}
		cursor, skip = c, s
		if remaining == 0 {
			break
		}
		if len(page) != 3 {
			t.Fatalf("page of %d with %d remaining", len(page), remaining)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("synced %d, want %d", len(seen), len(want))
	}
	for i, e := range want {
		if seen[i] != e.ID() {
			t.Errorf("entry %d = %s, want %s (%s)", i, seen[i], e.ID(), bodyOf(e))
		}
	}

	// From the start again: everything was acknowledged, so nothing.
	if page, _, _, remaining, _ := r.inetSync(dev, syncReq(t, 0, 0, 0), 50); len(page) != 0 || remaining != 0 {
		t.Errorf("acknowledged messages synced again: %d, %d remaining", len(page), remaining)
	}
	// ALL brings them back, and SENT adds the callsign's own.
	page, _, _, _, _ := r.inetSync(dev, syncReq(t, envelope.SyncFlagAll|envelope.SyncFlagSent, 0, 0), 50)
	if len(page) != len(want)+1 || page[len(page)-1].ID() != sent.ID() {
		t.Errorf("ALL|SENT synced %d, last %s", len(page), page[len(page)-1])
	}
}

// withECD sets Extended Callsign Data on a packet or stream datagram's LSF
// and fixes its CRCs.
func withECD(t *testing.T, b []byte, slot1, slot2 envelope.Address) []byte {
	t.Helper()
	b = append([]byte(nil), b...)
	s1, s2 := m17.EncodedCallsign(slot1.Bytes()), m17.EncodedCallsign(slot2.Bytes())
	var p2 *m17.EncodedCallsign
	if slot2 != 0 {
		p2 = &s2
	}
	switch string(b[:4]) {
	case magicM17P:
		p, err := m17.NewPacketFromBytes(b[4:])
		if err != nil {
			t.Fatal(err)
		}
		p.LSF.SetECD(&s1, p2)
		p.CalcCRC()
		return append([]byte(magicM17P), p.ToBytes()...)
	case magicM17S:
		sd, err := m17.NewStreamDatagramFromBytes(b)
		if err != nil {
			t.Fatal(err)
		}
		sd.LSF.SetECD(&s1, p2)
		copy(b[6:6+lsfLen-2], sd.LSF.ToBytes()[:lsfLen-2])
		binary.BigEndian.PutUint16(b[len(b)-2:], m17.CRC(b[:len(b)-2]))
		return b
	}
	t.Fatalf("not a packet or stream datagram")
	return nil
}

// TestRelayedTrafficIgnored: a packet another gateway transmitted from the
// network (marked by its ECD) is not taken in, and relayed voice is not
// heard; a repeater's local repeat still counts. An SMS identical to one
// taken in a moment ago is dropped.
func TestRelayedTrafficIgnored(t *testing.T) {
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face := startNativeFace(t, core, nil) // loopback is a gateway
	gwCall, other, refl := mustAddr(t, "N1ADJ  G"), mustAddr(t, "K1ABC  G"), mustAddr(t, "M17-QTC")
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	gw := newUDPPeer(t)
	gw.sendTo(t, face.Addr(), connDatagram(gwCall, 'A'))
	gw.expect(t, magicACKN)

	// Relayed: another gateway's SMS from the network, and reflector voice.
	gw.sendTo(t, face.Addr(), withECD(t, smsDatagram(ht, w1aw, "round and round"), other, refl))
	gw.sendTo(t, face.Addr(), withECD(t, streamDatagram(ht, w1aw), w1aw, refl))
	native := mustMsg(t, w1aw, ht, 1, 60, 1, 0, "native, relayed")
	gw.sendTo(t, face.Addr(), withECD(t, buildPacketDatagram(ht, w1aw, native.Bytes()), other, refl))
	time.Sleep(300 * time.Millisecond)
	core.mu.Lock()
	if len(core.sent) != 0 || len(core.heard) != 0 {
		t.Errorf("relayed traffic taken in: sent %d, heard %v", len(core.sent), core.heard)
	}
	core.mu.Unlock()

	// A repeater's local repeat names the source in slot 1 only: local.
	gw.sendTo(t, face.Addr(), withECD(t, streamDatagram(w1aw, ht), ht, 0))
	eventually(t, "local repeat heard", 2*time.Second, func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return len(core.heard) == 1 && core.heard[0] == ht
	})
	gw.sendTo(t, face.Addr(), withECD(t, smsDatagram(w1aw, ht, "via the repeater"), ht, 0))
	eventually(t, "local repeat taken in", 2*time.Second, func() bool { return core.sentCount() == 1 })

	// The same SMS again (a loop through a gateway that does not mark
	// what it relays): dropped. Different text: taken.
	gw.sendTo(t, face.Addr(), smsDatagram(w1aw, ht, "via the repeater"))
	gw.sendTo(t, face.Addr(), smsDatagram(w1aw, ht, "something new"))
	eventually(t, "new text taken in", 2*time.Second, func() bool { return core.sentCount() == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := core.sentCount(); n != 2 {
		t.Errorf("%d SMS taken in, want 2", n)
	}
}
