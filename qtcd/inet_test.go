package qtcd

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/jancona/m17"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jancona/qtc/envelope"
)

// stubCore records what the client face asks of the station.
type stubCore struct {
	mu       sync.Mutex
	heard    []envelope.Address
	vias     []Via
	sent     []*envelope.Envelope
	roomReqs []*envelope.Envelope
	joined   []envelope.Address
	relinked []envelope.Address
	node     envelope.Address
	local    envelope.Address

	// Native devices.
	acked      []ackEvent
	lost       []ackEvent
	syncPage   []*envelope.Envelope
	syncReqs   []envelope.Sync
	subscribed bool
}

type ackEvent struct {
	device envelope.Address
	msg    *envelope.Envelope
	rcpt   *envelope.Envelope
	fresh  bool
}

func (c *stubCore) inetAcked(d envelope.Address, msg, rcpt *envelope.Envelope, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acked = append(c.acked, ackEvent{d, msg, rcpt, fresh})
}
func (c *stubCore) inetLost(d envelope.Address, msg *envelope.Envelope, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lost = append(c.lost, ackEvent{device: d, msg: msg, fresh: fresh})
}
func (c *stubCore) inetSync(_ envelope.Address, req envelope.Sync, limit int) ([]*envelope.Envelope, uint32, uint16, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncReqs = append(c.syncReqs, req)
	page := c.syncPage[:min(limit, len(c.syncPage))]
	return page, 1234, uint16(len(page)), len(c.syncPage) - len(page), nil
}
func (c *stubCore) inetReachable(envelope.Address) bool { return true }
func (c *stubCore) inetSubscribed(envelope.Address, envelope.Address) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subscribed
}

func (c *stubCore) inetHeard(d envelope.Address, via Via, _ uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heard = append(c.heard, d)
	c.vias = append(c.vias, via)
	return nil
}
func (c *stubCore) inetRelinked(devices []envelope.Address) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.relinked = append(c.relinked, devices...)
}
func (c *stubCore) inetSend(e *envelope.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, e)
	return nil
}
func (c *stubCore) inetRoom(_ envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roomReqs = append(c.roomReqs, req)
	r, _ := req.Room()
	switch r.Op() {
	case envelope.OpJoin:
		c.joined = append(c.joined, r.Rooms()...)
	case envelope.OpList:
		return envelope.BuildRoom(envelope.OpOK, 1, c.joined, "")
	}
	return envelope.BuildRoom(envelope.OpOK, 1, nil, "")
}
func (c *stubCore) inetCallsign() envelope.Address  { return c.node }
func (c *stubCore) inetLocalRoom() envelope.Address { return c.local }
func (c *stubCore) inetDefaultTTL() uint16          { return 60 }
func (c *stubCore) inetLog() *slog.Logger           { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (c *stubCore) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// udpPeer is a fake gateway or fake reflector: a socket with a receive queue.
type udpPeer struct {
	conn *net.UDPConn
	recv chan []byte
	from chan *net.UDPAddr
}

func newUDPPeer(t *testing.T) *udpPeer { t.Helper(); return newUDPPeerAt(t, 0) }

// newUDPPeerAt listens on a given loopback port; 0 picks one.
func newUDPPeerAt(t *testing.T, port int) *udpPeer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	p := &udpPeer{conn: conn, recv: make(chan []byte, 64), from: make(chan *net.UDPAddr, 64)}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			p.recv <- append([]byte(nil), buf[:n]...)
			p.from <- addr
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return p
}

func (p *udpPeer) addr() *net.UDPAddr { return p.conn.LocalAddr().(*net.UDPAddr) }

func (p *udpPeer) sendTo(t *testing.T, to *net.UDPAddr, b []byte) {
	t.Helper()
	if _, err := p.conn.WriteToUDP(b, to); err != nil {
		t.Fatal(err)
	}
}

// expect waits for a datagram with the given magic, returning it and its sender.
func (p *udpPeer) expect(t *testing.T, magic string) ([]byte, *net.UDPAddr) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case b := <-p.recv:
			from := <-p.from
			if string(b[:4]) == magic {
				return b, from
			}
		case <-deadline:
			t.Fatalf("no %s datagram received", magic)
		}
	}
}

func (p *udpPeer) expectNone(t *testing.T, magic string, wait time.Duration) {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case b := <-p.recv:
			<-p.from
			if string(b[:4]) == magic {
				t.Fatalf("unexpected %s datagram: % x", magic, b)
			}
		case <-deadline:
			return
		}
	}
}

func connDatagram(callsign envelope.Address, module byte) []byte {
	b := controlDatagram(magicCONN, callsign)
	return append(b, module)
}

func smsDatagram(dst, src envelope.Address, text string) []byte {
	payload := append([]byte{byte(envelope.TypeSMS)}, text...)
	return buildPacketDatagram(dst, src, append(payload, 0))
}

func streamDatagram(dst, src envelope.Address) []byte {
	b := make([]byte, streamDgLen)
	copy(b, magicM17S)
	d, s := dst.Bytes(), src.Bytes()
	copy(b[6:12], d[:])
	copy(b[12:18], s[:])
	binary.BigEndian.PutUint16(b[streamDgLen-2:], m17.CRC(b[:streamDgLen-2]))
	return b
}

func TestM17Frame(t *testing.T) {
	// M17 CRC check value from the specification: "123456789" -> 0x772B.
	if got := m17.CRC([]byte("123456789")); got != 0x772B {
		t.Errorf("CRC(123456789) = %#x, want 0x772b", got)
	}
	dst, src := mustAddr(t, "W1AW"), mustAddr(t, "N1ADJ  H")
	dg := smsDatagram(dst, src, "hi")
	pf, err := parsePacketDatagram(dg)
	if err != nil {
		t.Fatal(err)
	}
	if pf.dst != dst || pf.src != src || pf.typ != envelope.TypeSMS || string(pf.payload[1:]) != "hi\x00" {
		t.Errorf("parsed %+v", pf)
	}
	dg[len(dg)-1] ^= 1
	if _, err := parsePacketDatagram(dg); err == nil {
		t.Error("accepted a bad payload CRC")
	}
	if _, s, rel, ok := streamAddrs(streamDatagram(dst, src)); !ok || s != src || rel {
		t.Error("streamAddrs failed")
	}
}

func TestInetFace(t *testing.T) {
	// Fast keepalives, set before any session goroutine reads them.
	oldRetry, oldPing := connRetryInterval, clientPingInterval
	connRetryInterval, clientPingInterval = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { connRetryInterval, clientPingInterval = oldRetry, oldPing })
	upstream := newUDPPeer(t) // the fake M17-M17
	gateway := newUDPPeer(t)  // the fake hotspot gateway
	node := mustAddr(t, "N1ADJ  Z")
	core := &stubCore{node: node, local: mustRoom(t, "N1ADJ")}
	face, err := newInetFace(core, InetConfig{
		Listen: "127.0.0.1:0",
		Modules: map[byte]ModuleConfig{
			'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC},
			'B': {Reflector: upstream.addr().String(), Module: 'D', Mode: ModeNative},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { face.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	station := face.Addr()

	gwCall := mustAddr(t, "N1ADJ  G")
	ht := mustAddr(t, "N1ADJ  H")
	w1aw := mustAddr(t, "W1AW")

	// Unmapped module: NACK, nothing upstream.
	gateway.sendTo(t, station, connDatagram(gwCall, 'Z'))
	gateway.expect(t, magicNACK)
	upstream.expectNone(t, magicCONN, 200*time.Millisecond)

	// QTC module A: CONN forwarded with the upstream module letter; resent
	// until the reflector answers; ACKN back.
	gateway.sendTo(t, station, connDatagram(gwCall, 'A'))
	gateway.expect(t, magicACKN) // answered by the node itself
	conn, upFrom := upstream.expect(t, magicCONN)
	if conn[10] != 'C' || envelope.AddressFromBytes(conn[4:10]) != gwCall {
		t.Errorf("upstream CONN = % x", conn)
	}
	upstream.expect(t, magicCONN) // the retry
	upstream.sendTo(t, upFrom, controlDatagram(magicNACK, 0))
	upstream.expect(t, magicCONN) // NACK is not final: retried
	upstream.sendTo(t, upFrom, controlDatagram(magicACKN, 0))
	upstream.expectNone(t, magicCONN, 500*time.Millisecond)
	gateway.expectNone(t, magicNACK, 100*time.Millisecond)

	// Stream frames are forwarded upstream and register presence via RF (loopback client).
	gateway.sendTo(t, station, streamDatagram(w1aw, ht))
	upstream.expect(t, magicM17S)
	eventually(t, "heard from stream", 2*time.Second, func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return len(core.heard) == 1 && core.heard[0] == ht && core.vias[0] == ViaRF
	})
	// The node answers reflector PINGs itself and PINGs the gateway itself;
	// neither side's keepalive crosses the node.
	upstream.sendTo(t, upFrom, controlDatagram(magicPING, 0))
	pong, _ := upstream.expect(t, magicPONG)
	if envelope.AddressFromBytes(pong[4:10]) != gwCall {
		t.Errorf("upstream PONG carries %s, want the gateway callsign", envelope.AddressFromBytes(pong[4:10]))
	}
	ping, _ := gateway.expect(t, magicPING)
	if envelope.AddressFromBytes(ping[4:10]) != node {
		t.Errorf("node PING carries %s", envelope.AddressFromBytes(ping[4:10]))
	}
	gateway.sendTo(t, station, controlDatagram(magicPONG, gwCall))
	upstream.expectNone(t, magicPONG, 200*time.Millisecond)

	// SMS on a qtc module becomes a MSG and is not forwarded.
	gateway.sendTo(t, station, smsDatagram(w1aw, ht, "  hello qtc  "))
	eventually(t, "SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 1 })
	upstream.expectNone(t, magicM17P, 200*time.Millisecond)
	core.mu.Lock()
	m, _ := core.sent[0].Msg()
	if core.sent[0].Source() != ht || core.sent[0].Destination() != w1aw || m.Body() != "hello qtc" || m.TTL() != 60 {
		t.Errorf("ingested %s", core.sent[0])
	}
	core.mu.Unlock()

	// SMS to the node callsign: a command gets a reply SMS from the node;
	// plain text goes to the local room.
	gateway.sendTo(t, station, smsDatagram(node, ht, "/join MAINE"))
	reply, _ := gateway.expect(t, magicM17P)
	pf, err := parsePacketDatagram(reply)
	if err != nil || pf.src != node || pf.dst != ht || pf.typ != envelope.TypeSMS || !bytes.HasPrefix(pf.payload[1:], []byte("joined MAINE")) {
		t.Errorf("command reply = %+v %v", pf, err)
	}
	gateway.sendTo(t, station, smsDatagram(node, ht, "/rooms"))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if !bytes.HasPrefix(pf.payload[1:], []byte("rooms: MAINE")) {
		t.Errorf("/rooms reply = %q", pf.payload[1:])
	}
	gateway.sendTo(t, station, smsDatagram(node, ht, "/join #MAINE"))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if !bytes.HasPrefix(pf.payload[1:], []byte("joined MAINE")) {
		t.Errorf("/join #MAINE reply = %q", pf.payload[1:])
	}
	// "#ROOM text" to the node addresses a named room; the marker and name
	// are stripped from the body. A bad name is answered with an error.
	gateway.sendTo(t, station, smsDatagram(node, ht, "#MAINE net tonight"))
	eventually(t, "room-addressed SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 2 })
	core.mu.Lock()
	if m, _ := core.sent[1].Msg(); core.sent[1].Destination() != mustRoom(t, "MAINE") || m.Body() != "net tonight" {
		t.Errorf("room-addressed SMS became %s", core.sent[1])
	}
	core.mu.Unlock()
	gateway.sendTo(t, station, smsDatagram(node, ht, "#MA.INE bad"))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if !bytes.HasPrefix(pf.payload[1:], []byte("error:")) {
		t.Errorf("bad room prefix reply = %q", pf.payload[1:])
	}
	gateway.sendTo(t, station, smsDatagram(node, ht, "everyone here?"))
	eventually(t, "local room SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 3 })
	core.mu.Lock()
	if core.sent[2].Destination() != core.local {
		t.Errorf("local room message went to %s", core.sent[2].Destination())
	}
	core.mu.Unlock()

	// A QTC MSG from a device (a native one, not the HT) is ingested and
	// acknowledged; a ROOM packet is answered with a ROOM reply packet.
	hn := mustAddr(t, "N1ADJ  N")
	native := mustMsg(t, hn, w1aw, 1, 60, 9, 0, "native")
	gateway.sendTo(t, station, buildPacketDatagram(w1aw, hn, native.Bytes()))
	eventually(t, "MSG ingested", 2*time.Second, func() bool { return core.sentCount() == 4 })
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if e, err := envelope.Parse(pf.payload); err != nil || e.Kind() != envelope.KindACK || pf.dst != hn {
		t.Errorf("MSG answered with %v %v to %s", e, err, pf.dst)
	}
	gateway.sendTo(t, station, buildPacketDatagram(node, hn, mustRoomPkt(t, envelope.OpList, 0).Bytes()))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if e, err := envelope.Parse(pf.payload); err != nil || e.Kind() != envelope.KindROOM {
		t.Errorf("ROOM reply = %v %v", e, err)
	}

	// Upstream messaging packets are dropped on a qtc module; voice passes.
	upstream.sendTo(t, upFrom, smsDatagram(ht, w1aw, "from the reflector"))
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)
	upstream.sendTo(t, upFrom, streamDatagram(ht, w1aw))
	gateway.expect(t, magicM17S)

	// Delivery: a MSG for the HT goes out as SMS to the gateway; a RCPT does not.
	face.deliver(ht, mustMsg(t, w1aw, ht, 1, 60, 3, 0, "for the HT"))
	out, _ := gateway.expect(t, magicM17P)
	pf, err = parsePacketDatagram(out)
	if err != nil || pf.dst != ht || pf.src != w1aw || pf.typ != envelope.TypeSMS || string(pf.payload[1:]) != "for the HT\x00" {
		t.Errorf("delivered %+v %v", pf, err)
	}
	// A room message is delivered to the device with the room named in the text.
	face.deliver(ht, mustMsg(t, w1aw, mustRoom(t, "MAINE"), 1, 60, 6, 0, "net tonight"))
	out, _ = gateway.expect(t, magicM17P)
	pf, err = parsePacketDatagram(out)
	if err != nil || pf.dst != ht || pf.src != w1aw || string(pf.payload[1:]) != "#MAINE net tonight\x00" {
		t.Errorf("room delivery %+v %v", pf, err)
	}
	rc, _ := envelope.BuildRcpt(w1aw, ht, envelope.ID{}, envelope.StatusDelivered, 1, 0, "")
	face.deliver(ht, rc)
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)
	// Unknown device: nothing sent.
	face.deliver(mustAddr(t, "AB1CD"), mustMsg(t, w1aw, mustAddr(t, "AB1CD"), 1, 60, 4, 0, "x"))
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)

	// Relink on native module B: SMS passes through both ways, no presence.
	core.mu.Lock()
	heardBefore := len(core.heard)
	core.mu.Unlock()
	gateway.sendTo(t, station, connDatagram(gwCall, 'B'))
	conn, upFrom = upstream.expect(t, magicCONN)
	if conn[10] != 'D' {
		t.Errorf("native CONN module %c", conn[10])
	}
	gateway.sendTo(t, station, smsDatagram(w1aw, ht, "native sms"))
	upstream.expect(t, magicM17P)
	upstream.sendTo(t, upFrom, smsDatagram(ht, w1aw, "reflector sms"))
	gateway.expect(t, magicM17P)
	gateway.sendTo(t, station, streamDatagram(w1aw, ht))
	upstream.expect(t, magicM17S)
	time.Sleep(100 * time.Millisecond)
	core.mu.Lock()
	if len(core.heard) != heardBefore || len(core.sent) != 4 {
		t.Errorf("native module published presence or ingested: heard %d sent %d", len(core.heard), len(core.sent))
	}
	core.mu.Unlock()
	// The device is no longer deliverable after the qtc session closed.
	face.deliver(ht, mustMsg(t, w1aw, ht, 1, 60, 5, 0, "gone"))
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)

	// DISC is forwarded upstream and ends the session.
	gateway.sendTo(t, station, controlDatagram(magicDISC, gwCall))
	upstream.expect(t, magicDISC)
	face.mu.Lock()
	n := len(face.sessions)
	face.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions after DISC", n)
	}
}

func TestInetAllowCallsigns(t *testing.T) {
	upstream := newUDPPeer(t)
	start := func(gateways []*net.IPNet) (*stubCore, *net.UDPAddr) {
		core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
		face, err := newInetFace(core, InetConfig{
			Listen:         "127.0.0.1:0",
			Gateways:       gateways,
			AllowCallsigns: []string{"N1ADJ", "w1aw"},
			Modules:        map[byte]ModuleConfig{'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { face.run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		return core, face.Addr()
	}
	ht, w1aw, stranger := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW"), mustAddr(t, "AB1CD")

	// No gateway ranges: the loopback client is an internet client.
	core, station := start([]*net.IPNet{})
	client := newUDPPeer(t)
	client.sendTo(t, station, connDatagram(stranger, 'A'))
	client.expect(t, magicNACK)
	upstream.expectNone(t, magicCONN, 200*time.Millisecond)

	client.sendTo(t, station, connDatagram(ht, 'A')) // a device of an allowed base
	client.expect(t, magicACKN)
	upstream.expect(t, magicCONN)

	// On the allowed session, a packet claiming a stranger's source is
	// dropped; stream frames from it pass but register no presence.
	client.sendTo(t, station, smsDatagram(w1aw, stranger, "spoofed"))
	client.sendTo(t, station, streamDatagram(w1aw, stranger))
	upstream.expect(t, magicM17S)
	client.sendTo(t, station, smsDatagram(w1aw, ht, "legit"))
	eventually(t, "allowed SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 1 })
	core.mu.Lock()
	if core.sent[0].Source() != ht {
		t.Errorf("ingested %s", core.sent[0])
	}
	if len(core.heard) != 1 || core.heard[0] != ht || core.vias[0] != ViaInternet {
		t.Errorf("heard %v via %v; want only %s via internet", core.heard, core.vias, ht)
	}
	core.mu.Unlock()

	// Gateways are not limited.
	core, station = start(nil) // default ranges include loopback
	gw := newUDPPeer(t)
	gw.sendTo(t, station, connDatagram(stranger, 'A'))
	gw.expect(t, magicACKN)
	gw.sendTo(t, station, smsDatagram(w1aw, stranger, "from RF"))
	eventually(t, "gateway SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 1 })

	if _, err := newInetFace(&stubCore{}, InetConfig{Listen: "127.0.0.1:0", AllowCallsigns: []string{"#NET"},
		Modules: map[byte]ModuleConfig{'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC}}}); err == nil {
		t.Error("accepted a non-callsign in AllowCallsigns")
	}
}

// TestInetClientHeardWhileLinked: an internet client on a qtc module is
// heard as soon as it links, without sending anything; a gateway is not.
func TestInetClientHeardWhileLinked(t *testing.T) {
	upstream := newUDPPeer(t)
	start := func(gateways []*net.IPNet) (*stubCore, *net.UDPAddr) {
		core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
		face, err := newInetFace(core, InetConfig{
			Listen:   "127.0.0.1:0",
			Gateways: gateways,
			Modules:  map[byte]ModuleConfig{'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { face.run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		return core, face.Addr()
	}
	me := mustAddr(t, "W1AW")

	core, station := start([]*net.IPNet{}) // loopback is an internet client
	client := newUDPPeer(t)
	client.sendTo(t, station, connDatagram(me, 'A'))
	client.expect(t, magicACKN)
	eventually(t, "client heard on link", 2*time.Second, func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return len(core.heard) == 1 && core.heard[0] == me && core.vias[0] == ViaInternet
	})

	core, station = start(nil) // loopback is a gateway
	gw := newUDPPeer(t)
	gw.sendTo(t, station, connDatagram(me, 'A'))
	gw.expect(t, magicACKN)
	time.Sleep(200 * time.Millisecond)
	core.mu.Lock()
	n := len(core.heard)
	core.mu.Unlock()
	if n != 0 {
		t.Errorf("gateway link heard %d devices; only what it carries should count", n)
	}
}

// TestGatewayRelinkKeepsRadios: when a gateway's link closes and it links
// again (a restart), the radios it carried are reattached to the new link.
func TestGatewayRelinkKeepsRadios(t *testing.T) {
	upstream := newUDPPeer(t)
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face, err := newInetFace(core, InetConfig{
		Listen:  "127.0.0.1:0",
		Modules: map[byte]ModuleConfig{'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { face.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	station := face.Addr()
	gwCall, ht, w1aw := mustAddr(t, "N1ADJ  G"), mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")

	gw := newUDPPeer(t)
	gw.sendTo(t, station, connDatagram(gwCall, 'A'))
	gw.expect(t, magicACKN)
	gw.sendTo(t, station, smsDatagram(w1aw, ht, "hello"))
	eventually(t, "radio heard", 2*time.Second, func() bool { return core.sentCount() == 1 })
	gw.sendTo(t, station, controlDatagram(magicDISC, gwCall))
	eventually(t, "session closed", 2*time.Second, func() bool {
		face.mu.Lock()
		defer face.mu.Unlock()
		return len(face.sessions) == 0
	})
	if face.deliver(ht, mustMsg(t, w1aw, ht, 1, 60, 1, 0, "while down")) != deliverNone {
		t.Error("delivered with no link")
	}

	// Another gateway linking does not take the radio.
	other := newUDPPeer(t)
	other.sendTo(t, station, connDatagram(mustAddr(t, "K1ABC  G"), 'A'))
	other.expect(t, magicACKN)

	// The same gateway relinks from a new port: the radio is back.
	gw2 := newUDPPeer(t)
	gw2.sendTo(t, station, connDatagram(gwCall, 'A'))
	gw2.expect(t, magicACKN)
	eventually(t, "relink reported", 2*time.Second, func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return len(core.relinked) == 1 && core.relinked[0] == ht
	})
	if face.deliver(ht, mustMsg(t, w1aw, ht, 1, 60, 2, 0, "after relink")) != deliverSent {
		t.Fatal("not delivered after relink")
	}
	b, _ := gw2.expect(t, magicM17P)
	if pf, err := parsePacketDatagram(b); err != nil || pf.dst != ht || string(pf.payload[1:]) != "after relink\x00" {
		t.Errorf("relinked delivery %+v %v", pf, err)
	}
	other.expectNone(t, magicM17P, 200*time.Millisecond)
}

// TestUpstreamSurvivesReadError: when the reflector goes away, CONN
// resends bounce and the upstream socket reports a read error. The session
// must keep reading, so that when the reflector returns it relinks and
// answers the reflector's PINGs, and it must back off its resends meanwhile.
func TestUpstreamSurvivesReadError(t *testing.T) {
	oldRetry, oldMax, oldSilence, oldPing := connRetryInterval, maxConnRetryInterval, upstreamSilence, clientPingInterval
	connRetryInterval, maxConnRetryInterval, upstreamSilence, clientPingInterval = 50*time.Millisecond, 400*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		connRetryInterval, maxConnRetryInterval, upstreamSilence, clientPingInterval = oldRetry, oldMax, oldSilence, oldPing
	})

	upstream := newUDPPeer(t)
	port := upstream.addr().Port
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face, err := newInetFace(core, InetConfig{
		Listen:  "127.0.0.1:0",
		Modules: map[byte]ModuleConfig{'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { face.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	gw := newUDPPeer(t)
	gw.sendTo(t, face.Addr(), connDatagram(mustAddr(t, "N1ADJ  G"), 'A'))
	gw.expect(t, magicACKN)
	_, upFrom := upstream.expect(t, magicCONN)
	upstream.sendTo(t, upFrom, controlDatagram(magicACKN, 0))
	upstream.expectNone(t, magicCONN, 150*time.Millisecond)

	// The reflector goes away. Keep the gateway's own link alive meanwhile.
	upstream.conn.Close()
	stopPong := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopPong:
				return
			case <-time.After(50 * time.Millisecond):
				gw.conn.WriteToUDP(controlDatagram(magicPONG, mustAddr(t, "N1ADJ  G")), face.Addr())
			}
		}
	}()
	defer close(stopPong)
	time.Sleep(1500 * time.Millisecond) // silence, relinking, bounced CONNs

	// It comes back on the same port: resent CONNs arrive, backed off.
	back := newUDPPeerAt(t, port)
	var conns []time.Time
	var from *net.UDPAddr
	for len(conns) < 3 {
		_, from = back.expect(t, magicCONN)
		conns = append(conns, time.Now())
	}
	if gap := conns[2].Sub(conns[1]); gap < 300*time.Millisecond {
		t.Errorf("CONN resent %v apart; want backed off toward %v", gap, maxConnRetryInterval)
	}
	back.sendTo(t, from, controlDatagram(magicACKN, 0))
	back.sendTo(t, from, controlDatagram(magicPING, 0))
	back.expect(t, magicPONG) // the reader is alive

	// Linked again: while the reflector keeps PINGing, no more CONNs.
	for i := 0; i < 5; i++ {
		back.sendTo(t, from, controlDatagram(magicPING, 0))
		back.expectNone(t, magicCONN, 100*time.Millisecond)
	}
}

func TestLoadHostsFile(t *testing.T) {
	path := t.TempDir() + "/hosts.txt"
	if err := writeFile(path, "# comment\n\nM17-M17 152.70.192.70 17000\nm17-kcw\t203.0.113.5\t17000\n"); err != nil {
		t.Fatal(err)
	}
	hosts, err := loadHostsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if hosts["M17-M17"].Addr != "152.70.192.70:17000" || hosts["M17-KCW"].Addr != "203.0.113.5:17000" {
		t.Errorf("hosts = %v", hosts)
	}
	if err := writeFile(path, "BAD LINE\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHostsFile(path); err == nil {
		t.Error("accepted a short line")
	}
	if _, err := newInetFace(&stubCore{}, InetConfig{Listen: "127.0.0.1:0", Modules: map[byte]ModuleConfig{'A': {Reflector: "M17-M17", Module: 'C', Mode: ModeQTC}}}); err == nil {
		t.Error("resolved a reflector name with no hosts file")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
