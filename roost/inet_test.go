package roost

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jancona/pigeon/envelope"
)

// stubCore records what the client face asks of the roost.
type stubCore struct {
	mu       sync.Mutex
	heard    []envelope.Address
	vias     []Via
	sent     []*envelope.Envelope
	roomReqs []*envelope.Envelope
	joined   []envelope.Address
	node     envelope.Address
	local    envelope.Address
}

func (c *stubCore) inetHeard(d envelope.Address, via Via, _ uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heard = append(c.heard, d)
	c.vias = append(c.vias, via)
	return nil
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

func newUDPPeer(t *testing.T) *udpPeer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
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
	binary.BigEndian.PutUint16(b[streamDgLen-2:], m17CRC(b[:streamDgLen-2]))
	return b
}

func TestM17Frame(t *testing.T) {
	// M17 CRC check value from the specification: "123456789" -> 0x772B.
	if got := m17CRC([]byte("123456789")); got != 0x772B {
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
	if _, s, ok := streamAddrs(streamDatagram(dst, src)); !ok || s != src {
		t.Error("streamAddrs failed")
	}
}

func TestInetFace(t *testing.T) {
	upstream := newUDPPeer(t) // the fake M17-M17
	gateway := newUDPPeer(t)  // the fake hotspot gateway
	node := mustAddr(t, "N1ADJ  Z")
	core := &stubCore{node: node, local: mustRoom(t, "N1ADJ")}
	face, err := newInetFace(core, InetConfig{
		Listen: "127.0.0.1:0",
		Modules: map[byte]ModuleConfig{
			'A': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModePigeon},
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
	roost := face.Addr()

	gwCall := mustAddr(t, "N1ADJ  G")
	ht := mustAddr(t, "N1ADJ  H")
	w1aw := mustAddr(t, "W1AW")

	// Unmapped module: NACK, nothing upstream.
	gateway.sendTo(t, roost, connDatagram(gwCall, 'Z'))
	gateway.expect(t, magicNACK)
	upstream.expectNone(t, magicCONN, 200*time.Millisecond)

	// Pigeon module A: CONN relayed with the upstream module letter; ACKN back.
	gateway.sendTo(t, roost, connDatagram(gwCall, 'A'))
	conn, upFrom := upstream.expect(t, magicCONN)
	if conn[10] != 'C' || envelope.AddressFromBytes(conn[4:10]) != gwCall {
		t.Errorf("upstream CONN = % x", conn)
	}
	upstream.sendTo(t, upFrom, controlDatagram(magicACKN, 0))
	gateway.expect(t, magicACKN)

	// Stream frames relay upstream and register presence via RF (loopback client).
	gateway.sendTo(t, roost, streamDatagram(w1aw, ht))
	upstream.expect(t, magicM17S)
	eventually(t, "heard from stream", 2*time.Second, func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return len(core.heard) == 1 && core.heard[0] == ht && core.vias[0] == ViaRF
	})
	// A reflector PING relays down; the gateway's PONG relays up.
	upstream.sendTo(t, upFrom, controlDatagram(magicPING, 0))
	gateway.expect(t, magicPING)
	gateway.sendTo(t, roost, controlDatagram(magicPONG, gwCall))
	upstream.expect(t, magicPONG)

	// SMS on a pigeon module becomes a MSG and is not forwarded.
	gateway.sendTo(t, roost, smsDatagram(w1aw, ht, "  hello pigeon  "))
	eventually(t, "SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 1 })
	upstream.expectNone(t, magicM17P, 200*time.Millisecond)
	core.mu.Lock()
	m, _ := core.sent[0].Msg()
	if core.sent[0].Source() != ht || core.sent[0].Destination() != w1aw || m.Body() != "hello pigeon" || m.TTL() != 60 {
		t.Errorf("ingested %s", core.sent[0])
	}
	core.mu.Unlock()

	// SMS to the node callsign: a command gets a reply SMS from the node;
	// plain text goes to the local room.
	gateway.sendTo(t, roost, smsDatagram(node, ht, "/join MAINE"))
	reply, _ := gateway.expect(t, magicM17P)
	pf, err := parsePacketDatagram(reply)
	if err != nil || pf.src != node || pf.dst != ht || pf.typ != envelope.TypeSMS || !bytes.HasPrefix(pf.payload[1:], []byte("joined MAINE")) {
		t.Errorf("command reply = %+v %v", pf, err)
	}
	gateway.sendTo(t, roost, smsDatagram(node, ht, "/rooms"))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if !bytes.HasPrefix(pf.payload[1:], []byte("rooms: MAINE")) {
		t.Errorf("/rooms reply = %q", pf.payload[1:])
	}
	gateway.sendTo(t, roost, smsDatagram(node, ht, "/join #MAINE"))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if !bytes.HasPrefix(pf.payload[1:], []byte("error:")) {
		t.Errorf("/join #MAINE reply = %q", pf.payload[1:])
	}
	gateway.sendTo(t, roost, smsDatagram(node, ht, "everyone here?"))
	eventually(t, "local room SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 2 })
	core.mu.Lock()
	if core.sent[1].Destination() != core.local {
		t.Errorf("local room message went to %s", core.sent[1].Destination())
	}
	core.mu.Unlock()

	// A native MSG packet from the client is ingested; a ROOM packet is
	// answered with a ROOM reply packet.
	native := mustMsg(t, ht, w1aw, 1, 60, 9, 0, "native")
	gateway.sendTo(t, roost, buildPacketDatagram(w1aw, ht, native.Bytes()))
	eventually(t, "MSG ingested", 2*time.Second, func() bool { return core.sentCount() == 3 })
	gateway.sendTo(t, roost, buildPacketDatagram(node, ht, mustRoomPkt(t, envelope.OpList, 0).Bytes()))
	reply, _ = gateway.expect(t, magicM17P)
	pf, _ = parsePacketDatagram(reply)
	if e, err := envelope.Parse(pf.payload); err != nil || e.Type() != envelope.TypeROOM {
		t.Errorf("ROOM reply = %v %v", e, err)
	}

	// Upstream messaging packets are dropped on a pigeon module; voice passes.
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
	rc, _ := envelope.BuildRcpt(w1aw, ht, envelope.ID{}, envelope.StatusDelivered, 1, 0, "")
	face.deliver(ht, rc)
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)
	// Unknown device: nothing sent.
	face.deliver(mustAddr(t, "AB1CD"), mustMsg(t, w1aw, mustAddr(t, "AB1CD"), 1, 60, 4, 0, "x"))
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)

	// Relink on native module B: SMS passes through both ways, no presence.
	gateway.sendTo(t, roost, connDatagram(gwCall, 'B'))
	conn, upFrom = upstream.expect(t, magicCONN)
	if conn[10] != 'D' {
		t.Errorf("native CONN module %c", conn[10])
	}
	gateway.sendTo(t, roost, smsDatagram(w1aw, ht, "native sms"))
	upstream.expect(t, magicM17P)
	upstream.sendTo(t, upFrom, smsDatagram(ht, w1aw, "reflector sms"))
	gateway.expect(t, magicM17P)
	gateway.sendTo(t, roost, streamDatagram(w1aw, ht))
	upstream.expect(t, magicM17S)
	time.Sleep(100 * time.Millisecond)
	core.mu.Lock()
	if len(core.heard) != 1 || len(core.sent) != 3 {
		t.Errorf("native module published presence or ingested: heard %d sent %d", len(core.heard), len(core.sent))
	}
	core.mu.Unlock()
	// The device is no longer deliverable after the pigeon session closed.
	face.deliver(ht, mustMsg(t, w1aw, ht, 1, 60, 5, 0, "gone"))
	gateway.expectNone(t, magicM17P, 200*time.Millisecond)

	// DISC relays upstream and ends the session.
	gateway.sendTo(t, roost, controlDatagram(magicDISC, gwCall))
	upstream.expect(t, magicDISC)
	face.mu.Lock()
	n := len(face.sessions)
	face.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions after DISC", n)
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
	if _, err := newInetFace(&stubCore{}, InetConfig{Listen: "127.0.0.1:0", Modules: map[byte]ModuleConfig{'A': {Reflector: "M17-M17", Module: 'C', Mode: ModePigeon}}}); err == nil {
		t.Error("resolved a reflector name with no hosts file")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
