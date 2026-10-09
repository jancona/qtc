package qtcd

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/qtc/envelope"
)

// startTestFace runs an inet face with the given modules, returning it and
// its stub core.
func startTestFace(t *testing.T, cfg InetConfig) (*inetFace, *stubCore) {
	t.Helper()
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	cfg.Listen = "127.0.0.1:0"
	face, err := newInetFace(core, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { face.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return face, core
}

func probeDatagram(probe m17.Probe, src envelope.Address) []byte {
	p := probe.Packet(encodedCallsign(src))
	return append([]byte(magicM17P), p.ToBytes()...)
}

func packetOf(t *testing.T, b []byte) m17.Packet {
	t.Helper()
	p, err := m17.NewPacketFromBytes(b[4:])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// streamFrame is a stream frame from src to dst with stream ID sid.
func streamFrame(t *testing.T, sid uint16, dst, src envelope.Address) []byte {
	t.Helper()
	lsf := m17.NewEmptyLSF()
	lsf.Dst = encodedCallsign(dst)
	lsf.Src = encodedCallsign(src)
	lsf.Type = [2]byte{0, 0x05} // stream, voice
	sd := m17.NewStreamDatagram(sid, 0, &lsf, make([]byte, 16))
	return sd.ToBytes()
}

func streamDst(t *testing.T, b []byte) string {
	t.Helper()
	sd, err := m17.NewStreamDatagramFromBytes(b)
	if err != nil {
		t.Fatalf("bad stream frame upstream: %v", err)
	}
	return sd.LSF.Dst.Callsign()
}

// TestNodeAnswersParrot: the node answers a gateway's probe itself, on a
// messaging-only module and on one with an upstream, so the gateway keeps
// sending it packets. The probe is not forwarded, and its source is not
// heard.
func TestNodeAnswersParrot(t *testing.T) {
	upstream := newUDPPeer(t) // never answers CONN, so the node never probes it
	face, core := startTestFace(t, InetConfig{Modules: map[byte]ModuleConfig{
		'A': {Mode: ModeQTC},
		'B': {Reflector: upstream.addr().String(), Module: 'C', Mode: ModeQTC},
		'N': {Reflector: upstream.addr().String(), Module: 'D', Mode: ModeNative},
	}})
	gwCall := mustAddr(t, "N1ADJ  G")
	for _, module := range []byte{'A', 'B', 'N'} {
		gw := newUDPPeer(t)
		gw.sendTo(t, face.Addr(), connDatagram(gwCall, module))
		gw.expect(t, magicACKN)
		probe := m17.NewProbe()
		gw.sendTo(t, face.Addr(), probeDatagram(probe, gwCall))
		b, _ := gw.expect(t, magicM17P)
		reply := packetOf(t, b)
		if !probe.IsReply(reply) || reply.LSF.Dst != m17.EncodedDestinationAllBytes || !reply.LSF.CheckCRC() || !reply.CheckCRC() {
			t.Errorf("module %c: reply %v; want the probe readdressed to broadcast", module, reply)
		}
	}
	upstream.expectNone(t, magicM17P, 200*time.Millisecond)
	core.mu.Lock()
	defer core.mu.Unlock()
	for _, d := range core.heard {
		if d == gwCall {
			t.Error("gateway's probe counted as hearing its callsign")
		}
	}
}

// linkUpstream links a gateway to module A of a face whose upstream is
// the named reflector URF587 module C. It returns the gateway, the
// upstream, the face, the node's address as the upstream sees it, and a
// function returning the next datagram the upstream receives.
func linkUpstream(t *testing.T) (*udpPeer, *udpPeer, *inetFace, *net.UDPAddr, func() ([]byte, bool)) {
	t.Helper()
	old := probeInterval
	probeInterval = 50 * time.Millisecond
	t.Cleanup(func() { probeInterval = old })
	upstream := newUDPPeer(t)
	hosts := filepath.Join(t.TempDir(), "M17Hosts.txt")
	if err := writeFile(hosts, fmt.Sprintf("URF587 127.0.0.1 %d\n", upstream.addr().Port)); err != nil {
		t.Fatal(err)
	}
	face, _ := startTestFace(t, InetConfig{HostsFile: hosts, Modules: map[byte]ModuleConfig{
		'A': {Reflector: "URF587", Module: 'C', Mode: ModeQTC},
	}})
	gw := newUDPPeer(t)
	gw.sendTo(t, face.Addr(), connDatagram(mustAddr(t, "N1ADJ  G"), 'A'))
	gw.expect(t, magicACKN)
	_, upFrom := upstream.expect(t, magicCONN)
	upstream.sendTo(t, upFrom, controlDatagram(magicACKN, 0))
	next := func() ([]byte, bool) {
		select {
		case b := <-upstream.recv:
			<-upstream.from
			return b, true
		case <-time.After(time.Second):
			return nil, false
		}
	}
	return gw, upstream, face, upFrom, next
}

// nextProbe returns the next probe the node sends upstream, skipping
// CONN resends.
func nextProbe(t *testing.T, next func() ([]byte, bool)) m17.Packet {
	t.Helper()
	for {
		b, ok := next()
		if !ok {
			t.Fatal("no probe sent upstream")
		}
		if string(b[:4]) != magicM17P {
			continue
		}
		p := packetOf(t, b)
		if p.LSF.Dst.Callsign() != "PARROT" || p.Type != m17.PacketTypeRAW {
			t.Fatalf("upstream packet %v, want a probe", p)
		}
		if got := envelope.AddressFromBytes(p.LSF.Src[:]); got != mustAddr(t, "N1ADJ  G") {
			t.Errorf("probe from %s, want the gateway callsign", got)
		}
		return p
	}
}

// TestUpstreamLegacy: an upstream that doesn't answer three probes is
// legacy: broadcast streams are addressed to it, directed ones go as they
// are, and packets aren't forwarded. A late reply makes it current again,
// and isn't passed to the gateway.
func TestUpstreamLegacy(t *testing.T) {
	gw, upstream, face, upFrom, next := linkUpstream(t)
	first := nextProbe(t, next)
	nextProbe(t, next)
	nextProbe(t, next)
	time.Sleep(3 * probeInterval)
	upstream.expectNone(t, magicM17P, 100*time.Millisecond) // no fourth probe

	w1aw, ht := mustAddr(t, "W1AW"), mustAddr(t, "N1ADJ  H")
	gw.sendTo(t, face.Addr(), streamFrame(t, 1, envelope.Broadcast, ht))
	b, _ := upstream.expect(t, magicM17S)
	if got := streamDst(t, b); got != "URF587  C" {
		t.Errorf("broadcast stream upstream to %q, want URF587  C", got)
	}
	gw.sendTo(t, face.Addr(), streamFrame(t, 2, w1aw, ht))
	b, _ = upstream.expect(t, magicM17S)
	if got := streamDst(t, b); got != "W1AW" {
		t.Errorf("directed stream upstream to %q, want W1AW", got)
	}
	aprs := buildPacketDatagram(w1aw, ht, []byte{byte(m17.PacketTypeAPRS), 'x'})
	gw.sendTo(t, face.Addr(), aprs)
	upstream.expectNone(t, magicM17P, 200*time.Millisecond)

	// The reply to the first probe arrives late.
	reply := m17.ParrotReply(first)
	upstream.sendTo(t, upFrom, append([]byte(magicM17P), reply.ToBytes()...))
	gw.expectNone(t, magicM17P, 200*time.Millisecond)
	gw.sendTo(t, face.Addr(), streamFrame(t, 3, envelope.Broadcast, ht))
	b, _ = upstream.expect(t, magicM17S)
	if got := streamDst(t, b); got != "@ALL" {
		t.Errorf("broadcast stream to a current upstream sent to %q, want @ALL", got)
	}
	gw.sendTo(t, face.Addr(), aprs)
	upstream.expect(t, magicM17P)
}

// TestUpstreamCurrent: an upstream that answers the probe is current:
// streams and packets go unchanged, and the reply isn't passed on.
func TestUpstreamCurrent(t *testing.T) {
	gw, upstream, face, upFrom, next := linkUpstream(t)
	probe := nextProbe(t, next)
	reply := m17.ParrotReply(probe)
	upstream.sendTo(t, upFrom, append([]byte(magicM17P), reply.ToBytes()...))
	gw.expectNone(t, magicM17P, 200*time.Millisecond)
	upstream.expectNone(t, magicM17P, 5*probeInterval) // not probed again

	w1aw, ht := mustAddr(t, "W1AW"), mustAddr(t, "N1ADJ  H")
	gw.sendTo(t, face.Addr(), streamFrame(t, 1, envelope.Broadcast, ht))
	b, _ := upstream.expect(t, magicM17S)
	if got := streamDst(t, b); got != "@ALL" {
		t.Errorf("broadcast stream upstream to %q, want @ALL", got)
	}
	gw.sendTo(t, face.Addr(), buildPacketDatagram(w1aw, ht, []byte{byte(m17.PacketTypeAPRS), 'x'}))
	upstream.expect(t, magicM17P)
}
