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

// expectQTC waits for a QTC packet and returns its LSF addresses and the
// parsed payload.
func (n *fakeNode) expectQTC(t *testing.T) (dst, src envelope.Address, e *envelope.Envelope) {
	t.Helper()
	b := n.expect(t, m17.MagicM17Packet)
	p := m17.NewPacketFromBytes(b[4:])
	if !p.LSF.CheckCRC() || !p.CheckCRC() || byte(p.Type) != byte(envelope.TypeQTC) {
		t.Fatalf("bad QTC packet % x", b)
	}
	e, err := envelope.Parse(append([]byte{byte(p.Type)}, p.Payload...))
	if err != nil {
		t.Fatal(err)
	}
	return envelope.AddressFromBytes(p.LSF.Dst[:]), envelope.AddressFromBytes(p.LSF.Src[:]), e
}

// expectKind waits for a QTC packet of one kind, skipping others.
func (n *fakeNode) expectKind(t *testing.T, k envelope.Kind) (envelope.Address, *envelope.Envelope) {
	t.Helper()
	for {
		dst, _, e := n.expectQTC(t)
		if e.Kind() == k {
			return dst, e
		}
	}
}

func controlWith(magic string, a envelope.Address) []byte {
	b := a.Bytes()
	return append([]byte(magic), b[:]...)
}

// qtcPacket frames a QTC payload from the node side.
func qtcPacket(t *testing.T, dst, src envelope.Address, e *envelope.Envelope) []byte {
	t.Helper()
	lsf, err := m17.NewLSF("N1ADJ", "N1ADJ", m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	if err != nil {
		t.Fatal(err)
	}
	lsf.Dst = m17.EncodedCallsign(dst.Bytes())
	lsf.Src = m17.EncodedCallsign(src.Bytes())
	lsf.CalcCRC()
	b := e.Bytes()
	p := m17.Packet{LSF: &lsf, Type: m17.PacketType(b[0]), Payload: b[1:]}
	p.CalcCRC()
	return append([]byte(m17.MagicM17Packet), p.ToBytes()...)
}

func addrOf(t *testing.T, s string) envelope.Address {
	t.Helper()
	a, err := envelope.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
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
	nodeCall := addrOf(t, "N1ADJ   P")
	me, w1aw, k1abc := addrOf(t, "N1ADJ"), addrOf(t, "W1AW"), addrOf(t, "K1ABC")
	c, err := newChat("n1adj", "a")
	if err != nil {
		t.Fatal(err)
	}
	c.connRetry = 100 * time.Millisecond
	c.ackTimeout, c.ackRetries, c.pageQuiet = 200*time.Millisecond, 1, 200*time.Millisecond
	c.statePath = t.TempDir() + "/sync.json"
	inR, inW := io.Pipe()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- c.run(context.Background(), node.addr(), inR, out) }()
	type_ := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	// expectMsg waits for a MSG from us and acknowledges it.
	expectMsg := func(to envelope.Address, body string) *envelope.Envelope {
		t.Helper()
		_, e := node.expectKind(t, envelope.KindMSG)
		m, _ := e.Msg()
		if e.Destination() != to || e.Source() != me || m.Body() != body {
			t.Fatalf("sent %s", e)
		}
		ack, _ := envelope.BuildAck([]envelope.ID{e.ID()})
		node.send(t, qtcPacket(t, me, nodeCall, ack))
		return e
	}

	// CONN carries our callsign and module; resent until answered.
	conn := node.expect(t, m17.MagicCONN)
	if cs, _ := m17.DecodeCallsign(conn[4:10]); cs != "N1ADJ" || conn[10] != 'A' {
		t.Errorf("CONN %q module %c", cs, conn[10])
	}
	node.expect(t, m17.MagicCONN)
	type_("@W1AW too soon")
	waitOutput(t, out, "Not linked; message not sent.")

	// Linking starts a sync from the beginning; a page of one message.
	node.send(t, controlWith(m17.MagicACKN, nodeCall))
	waitOutput(t, out, "Linked to node N1ADJ   P.")
	dst, e := node.expectKind(t, envelope.KindSYNC)
	if y, _ := e.Sync(); dst != nodeCall || y.Op() != envelope.SyncRequest || y.Cursor() != 0 || y.Skip() != 0 {
		t.Fatalf("sync request %s to %s", e, dst)
	}
	page, _ := envelope.BuildSync(envelope.SyncPage, 0, 500, 1, 1, 0)
	node.send(t, qtcPacket(t, me, nodeCall, page))
	missed, _ := envelope.BuildMsg(w1aw, me, uint32(time.Now().Unix()), 60, 1, envelope.FlagRcptReq, "while you were away")
	node.send(t, qtcPacket(t, me, w1aw, missed))
	waitOutput(t, out, " W1AW: while you were away")
	if _, e := node.expectKind(t, envelope.KindRCPT); e.Source() != me || e.Destination() != w1aw {
		t.Errorf("acknowledged with %s", e)
	} else if rc, _ := e.Rcpt(); rc.Status() != envelope.StatusDelivered || rc.MessageID() != missed.ID() {
		t.Errorf("acknowledged with %s", e)
	}
	eventually(t, "sync position saved", func() bool {
		b, err := os.ReadFile(c.statePath)
		return err == nil && string(b) == `{"cursor":500,"skip":1}`
	})

	// Direct message: resent until the node acknowledges it.
	type_("@w1aw hello there")
	_, first := node.expectKind(t, envelope.KindMSG)
	hello := expectMsg(w1aw, "hello there")
	if hello.ID() != first.ID() {
		t.Errorf("resend was a different message")
	}
	if m, _ := hello.Msg(); !m.RcptReq() {
		t.Error("direct message does not ask for receipts")
	}
	type_("again")
	expectMsg(w1aw, "again")
	// A bare @CALLSIGN switches the recipient without sending.
	type_("@k1abc")
	waitOutput(t, out, "Plain text now goes to K1ABC.")
	type_("switched")
	expectMsg(k1abc, "switched")
	type_("@#net hi")
	waitOutput(t, out, `"#net" is not a callsign`)
	// Room messages go to the room's address.
	net, _ := envelope.RoomAddress("NET")
	type_("#net hi all")
	expectMsg(net, "hi all")
	type_("more for the room")
	expectMsg(net, "more for the room")

	// Room commands are ROOM requests.
	maine, _ := envelope.RoomAddress("MAINE")
	type_("/join MAINE")
	dst, e = node.expectKind(t, envelope.KindROOM)
	if r, _ := e.Room(); dst != nodeCall || r.Op() != envelope.OpJoin || len(r.Rooms()) != 1 || r.Rooms()[0] != maine {
		t.Errorf("join %s to %s", e, dst)
	}
	ok, _ := envelope.BuildRoom(envelope.OpOK, 1, nil, "")
	node.send(t, qtcPacket(t, me, nodeCall, ok))
	waitOutput(t, out, "* joined")
	type_("/rooms")
	node.expectKind(t, envelope.KindROOM)
	list, _ := envelope.BuildRoom(envelope.OpOK, 1, []envelope.Address{maine, net}, "")
	node.send(t, qtcPacket(t, me, nodeCall, list))
	waitOutput(t, out, "* rooms: #MAINE #NET")

	// PING is answered with our callsign.
	node.send(t, controlWith(m17.MagicPING, nodeCall))
	if pong := node.expect(t, m17.MagicPONG); !bytes.Equal(pong[4:10], mustEncoded(t, "N1ADJ")) {
		t.Errorf("PONG % x", pong)
	}

	// Incoming: shown once however often it comes, acknowledged each time.
	back, _ := envelope.BuildMsg(w1aw, me, uint32(time.Now().Unix()), 60, 2, 0, "hi back")
	for range 2 {
		node.send(t, qtcPacket(t, me, w1aw, back))
		if _, e := node.expectKind(t, envelope.KindRCPT); e.Destination() != w1aw {
			t.Errorf("acknowledged with %s", e)
		}
	}
	waitOutput(t, out, " W1AW: hi back")
	if n := strings.Count(out.String(), "hi back"); n != 1 {
		t.Errorf("shown %d times", n)
	}
	eve, _ := envelope.BuildMsg(k1abc, net, uint32(time.Now().Unix()), 60, 3, 0, "evening all")
	node.send(t, qtcPacket(t, net, k1abc, eve))
	waitOutput(t, out, " #NET K1ABC: evening all")
	if _, e := node.expectKind(t, envelope.KindACK); e == nil {
		t.Error("room message not acknowledged")
	}
	fromNode, _ := envelope.BuildMsg(nodeCall, me, uint32(time.Now().Unix()), 60, 4, 0, "3 older messages not sent")
	node.send(t, qtcPacket(t, me, nodeCall, fromNode))
	waitOutput(t, out, " * 3 older messages not sent")

	// A receipt for something we sent.
	dl, _ := envelope.BuildRcpt(w1aw, me, hello.ID(), envelope.StatusDelivered, 1, 0, "")
	node.send(t, qtcPacket(t, me, w1aw, dl))
	waitOutput(t, out, `* W1AW received "hello there"`)

	// NOTIFY starts a sync from the saved position.
	notify, _ := envelope.BuildSync(envelope.SyncNotify, 0, 0, 0, 0, 4)
	node.send(t, qtcPacket(t, me, nodeCall, notify))
	_, e = node.expectKind(t, envelope.KindSYNC)
	if y, _ := e.Sync(); y.Op() != envelope.SyncRequest || y.Cursor() != 500 || y.Skip() != 1 {
		t.Errorf("sync after NOTIFY %s", e)
	}

	// Never acknowledged: reported as not sent.
	type_("@w1aw into the void")
	waitOutput(t, out, `not sent, the node did not answer: "into the void"`)

	type_("/quit")
	node.expect(t, m17.MagicDISC)
	if err := <-done; err != nil {
		t.Errorf("run = %v", err)
	}
	inW.Close()
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
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

func TestChatSaysWhenNoAnswer(t *testing.T) {
	node := newFakeNode(t) // listens, never answers
	c, _ := newChat("N1ADJ", "A")
	c.connRetry, c.noAnswer = 50*time.Millisecond, 200*time.Millisecond
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &syncBuffer{}
	go c.run(context.Background(), node.addr(), inR, out)
	waitOutput(t, out, "No answer from "+node.addr()+" yet; still trying.")
	time.Sleep(400 * time.Millisecond)
	if n := strings.Count(out.String(), "No answer from"); n != 1 {
		t.Errorf("warned %d times, want once", n)
	}
}
