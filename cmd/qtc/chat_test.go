package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/qtc/envelope"
)

// fakeNode is the node side of the client face: a UDP socket that records
// what the chat client sends.
type fakeNode struct {
	conn *net.UDPConn
	recv chan []byte
	mu   sync.Mutex
	peer *net.UDPAddr
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	n := &fakeNode{conn: conn, recv: make(chan []byte, 64)}
	go func() {
		buf := make([]byte, 2048)
		for {
			k, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.peer = from
			n.mu.Unlock()
			n.recv <- append([]byte(nil), buf[:k]...)
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return n
}

func (n *fakeNode) addr() string { return n.conn.LocalAddr().String() }

func (n *fakeNode) send(t *testing.T, b []byte) {
	t.Helper()
	n.mu.Lock()
	peer := n.peer
	n.mu.Unlock()
	if _, err := n.conn.WriteToUDP(b, peer); err != nil {
		t.Fatal(err)
	}
}

// expect waits for a datagram with the given magic.
func (n *fakeNode) expect(t *testing.T, magic string) []byte {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case b := <-n.recv:
			if string(b[:4]) == magic {
				return b
			}
		case <-deadline:
			t.Fatalf("no %s from the client", magic)
		}
	}
}

// expectSMS waits for an SMS and returns its destination and text.
func (n *fakeNode) expectSMS(t *testing.T) (dst, src envelope.Address, text string) {
	t.Helper()
	b := n.expect(t, m17.MagicM17Packet)
	p := m17.NewPacketFromBytes(b[4:])
	if !p.CheckCRC() || p.Type != m17.PacketTypeSMS {
		t.Fatalf("bad SMS packet % x", b)
	}
	return envelope.AddressFromBytes(p.LSF.Dst[:]), envelope.AddressFromBytes(p.LSF.Src[:]), strings.TrimSuffix(string(p.Payload), "\x00")
}

func controlWith(magic string, a envelope.Address) []byte {
	b := a.Bytes()
	return append([]byte(magic), b[:]...)
}

func sms(t *testing.T, dst, src, text string) []byte {
	t.Helper()
	p, err := m17.NewPacket(dst, src, m17.PacketTypeSMS, append([]byte(text), 0))
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(m17.MagicM17Packet), p.ToBytes()...)
}

// syncBuffer is an io.Writer the test can read while the client writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("output never showed %q:\n%s", want, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestChat(t *testing.T) {
	node := newFakeNode(t)
	nodeCall, _ := envelope.EncodeAddress("N1ADJ   P")
	c, err := newChat("n1adj", "a")
	if err != nil {
		t.Fatal(err)
	}
	c.connRetry = 100 * time.Millisecond
	inR, inW := io.Pipe()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- c.run(context.Background(), node.addr(), inR, out) }()
	type_ := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}

	// CONN carries our callsign and module; resent until answered.
	conn := node.expect(t, m17.MagicCONN)
	if cs, _ := m17.DecodeCallsign(conn[4:10]); cs != "N1ADJ" || conn[10] != 'A' {
		t.Errorf("CONN %q module %c", cs, conn[10])
	}
	node.expect(t, m17.MagicCONN)
	type_("W1AW: too soon")
	waitOutput(t, out, "Not linked; message not sent.")

	node.send(t, controlWith(m17.MagicACKN, nodeCall))
	waitOutput(t, out, "Linked to node N1ADJ   P.")

	// Direct message, then plain text to the same callsign.
	type_("w1aw: hello there")
	if dst, src, text := node.expectSMS(t); dst.String() != "W1AW" || src.String() != "N1ADJ" || text != "hello there" {
		t.Errorf("sent %s→%s %q", src, dst, text)
	}
	type_("again")
	if dst, _, text := node.expectSMS(t); dst.String() != "W1AW" || text != "again" {
		t.Errorf("plain text went to %s: %q", dst, text)
	}
	// Room message and room command go to the node's callsign.
	type_("#net hi all")
	if dst, _, text := node.expectSMS(t); dst != nodeCall || text != "#net hi all" {
		t.Errorf("room message to %s: %q", dst, text)
	}
	type_("more for the room")
	if dst, _, text := node.expectSMS(t); dst != nodeCall || text != "#NET more for the room" {
		t.Errorf("plain text to room: %s %q", dst, text)
	}
	type_("/join MAINE")
	if dst, _, text := node.expectSMS(t); dst != nodeCall || text != "/join MAINE" {
		t.Errorf("command to %s: %q", dst, text)
	}

	// PING is answered with our callsign.
	node.send(t, controlWith(m17.MagicPING, nodeCall))
	if pong := node.expect(t, m17.MagicPONG); !bytes.Equal(pong[4:10], mustEncoded(t, "N1ADJ")) {
		t.Errorf("PONG % x", pong)
	}

	// Incoming: direct, from the node, and a room message.
	node.send(t, sms(t, "N1ADJ", "W1AW", "hi back"))
	waitOutput(t, out, " W1AW: hi back")
	node.send(t, sms(t, "N1ADJ", "N1ADJ   P", "rooms: MAINE NET"))
	waitOutput(t, out, " * rooms: MAINE NET")
	node.send(t, sms(t, "N1ADJ", "K1ABC", "#NET evening all"))
	waitOutput(t, out, " #NET K1ABC: evening all")

	type_("/quit")
	node.expect(t, m17.MagicDISC)
	if err := <-done; err != nil {
		t.Errorf("run = %v", err)
	}
	inW.Close()
}

func TestChatNoTarget(t *testing.T) {
	c, _ := newChat("N1ADJ", "A")
	out := &syncBuffer{}
	c.out = out
	c.handleInput("hello")
	if !strings.Contains(out.String(), "Who to?") {
		t.Errorf("output %q", out.String())
	}
	c.handleInput("/to #bad.name")
	if !strings.Contains(out.String(), "is not a callsign or #ROOM") {
		t.Errorf("output %q", out.String())
	}
}

func TestChatRefused(t *testing.T) {
	node := newFakeNode(t)
	c, _ := newChat("N1ADJ", "A")
	done := make(chan error, 1)
	inR, inW := io.Pipe() // input that stays open
	defer inW.Close()
	go func() { done <- c.run(context.Background(), node.addr(), inR, io.Discard) }()
	node.expect(t, m17.MagicCONN)
	node.send(t, []byte(m17.MagicNACK))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("run = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after NACK")
	}
}

func TestLookupReflector(t *testing.T) {
	p := t.TempDir() + "/M17Hosts.txt"
	if err := os.WriteFile(p, []byte("# comment\nM17-M17 107.191.121.105 17000\nM17-QTC 127.0.0.1 17000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, err := lookupReflector(p, "m17-qtc"); err != nil || a != "127.0.0.1:17000" {
		t.Errorf("lookup = %q, %v", a, err)
	}
	if _, err := lookupReflector(p, "M17-XYZ"); err == nil {
		t.Error("found a missing reflector")
	}
}

func mustEncoded(t *testing.T, s string) []byte {
	t.Helper()
	cs, err := m17.EncodeCallsign(s)
	if err != nil {
		t.Fatal(err)
	}
	return cs[:]
}
