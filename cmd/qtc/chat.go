package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/qtc/envelope"
)

// qtc chat is a terminal client for a QTC node's client face (node protocol
// §10). It links to the node like a gateway or M17_inet client does and is
// a native client (docs/qtc-client.md): it sends and receives the QTC
// packet type, acknowledges what it receives, resends what the node has not
// acknowledged, and syncs what it missed each time it links. It speaks
// M17_inet itself, with framing from the m17 package, because it needs the
// node's callsign from the node's ACKN and PING.

const chatHelp = `Type a message:
  @W1AW hello         send to a callsign
  #NET hello          post to a room
  hello               send to whoever you last wrote to
Commands:
  /join NET           join a room          /leave NET   leave a room
  /rooms              list your rooms      /to W1AW     set who plain text goes to
  /help               this help            /quit        disconnect and exit`

// maxText keeps a message inside one QTC packet.
const maxText = envelope.MaxBodyUnsigned

func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	callsign := fs.String("callsign", "", "your callsign, e.g. N1ADJ (required)")
	node := fs.String("node", "", "the QTC node's address, host[:port]; port defaults to 17000")
	reflector := fs.String("reflector", "", "the node's name in -hosts, e.g. M17-QTC, instead of -node")
	hosts := fs.String("hosts", "/opt/m17/rpi-dashboard/files/M17Hosts.txt", "M17Hosts.txt, for -reflector")
	module := fs.String("module", "A", "the node's module to link to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *callsign == "" || (*node == "") == (*reflector == "") {
		fs.Usage()
		return errors.New("need -callsign, and one of -node or -reflector")
	}
	addr := *node
	if *reflector != "" {
		a, err := lookupReflector(*hosts, *reflector)
		if err != nil {
			return err
		}
		addr = a
	} else if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "17000")
	}
	c, err := newChat(*callsign, *module)
	if err != nil {
		return err
	}
	c.statePath = defaultStatePath(c.me)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return c.run(ctx, addr, os.Stdin, os.Stdout)
}

// lookupReflector finds name in an M17Hosts.txt ("NAME ADDRESS PORT" lines).
func lookupReflector(path, name string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && !strings.HasPrefix(f[0], "#") && strings.EqualFold(f[0], name) {
			return net.JoinHostPort(f[1], f[2]), nil
		}
	}
	return "", fmt.Errorf("%s is not in %s", name, path)
}

type chat struct {
	me     string // our callsign, as sent in CONN and as the source of what we send
	meAddr envelope.Address
	module byte

	// Link and retry timing; tests shorten them.
	connRetry   time.Duration
	pingTimeout time.Duration
	noAnswer    time.Duration // unanswered this long, say so once
	ackTimeout  time.Duration // resend an unacknowledged MSG after this
	ackRetries  int
	pageQuiet   time.Duration // a sync page is done after this long with nothing new

	// statePath keeps the sync position between runs; "" keeps nothing.
	statePath string

	addr        string
	unanswered  time.Time // when we started waiting for an answer; zero once linked
	warnedSince time.Time // the wait already warned about

	mu       sync.Mutex
	out      io.Writer
	conn     *net.UDPConn
	node     envelope.Address // learned from the node's ACKN or PING
	linked   bool
	lastPing time.Time
	to       string // where plain text goes: a callsign or "#ROOM"

	pending  map[envelope.ID]*outgoing // our MSGs awaiting the node's ACK
	sentText map[envelope.ID]string    // what we sent, for showing receipts
	seen     map[envelope.ID]bool      // MSGs already shown
	roomOp   []envelope.RoomOp         // ROOM requests awaiting replies, oldest first
	sync     syncState
}

// outgoing is a MSG we sent and the node has not acknowledged.
type outgoing struct {
	e        *envelope.Envelope
	attempts int
	next     time.Time
}

// syncState tracks the sync in progress (client spec §5).
type syncState struct {
	Cursor uint32 `json:"cursor"`
	Skip   uint16 `json:"skip"`

	active    bool
	requested time.Time // when the last REQUEST went out; zero once its PAGE came
	remaining uint16    // from the PAGE
	lastPkt   time.Time // last page packet, for pageQuiet
	next      struct {
		cursor uint32
		skip   uint16
	}
}

func newChat(callsign, module string) (*chat, error) {
	me := strings.ToUpper(strings.TrimSpace(callsign))
	a, err := envelope.EncodeAddress(me)
	if err != nil || !a.IsStandard() {
		return nil, fmt.Errorf("%q is not a callsign", callsign)
	}
	module = strings.ToUpper(module)
	if len(module) != 1 || module[0] < 'A' || module[0] > 'Z' {
		return nil, fmt.Errorf("module must be one letter A-Z")
	}
	return &chat{
		me: me, meAddr: a, module: module[0],
		connRetry: 5 * time.Second, pingTimeout: 30 * time.Second, noAnswer: 10 * time.Second,
		ackTimeout: 2 * time.Second, ackRetries: 3, pageQuiet: 3 * time.Second,
		pending: map[envelope.ID]*outgoing{}, sentText: map[envelope.ID]string{}, seen: map[envelope.ID]bool{},
	}, nil
}

// defaultStatePath is where the sync position is kept: per callsign, since
// a cursor is good at any node (client spec §5.2).
func defaultStatePath(callsign string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "qtc", "sync-"+strings.ReplaceAll(strings.ToUpper(callsign), "/", "_")+".json")
}

func (c *chat) loadState() {
	if c.statePath == "" {
		return
	}
	b, err := os.ReadFile(c.statePath)
	if err != nil {
		return
	}
	var st syncState
	if json.Unmarshal(b, &st) == nil {
		c.sync.Cursor, c.sync.Skip = st.Cursor, st.Skip
	}
}

// saveState records the sync position. Callers hold c.mu.
func (c *chat) saveState() {
	if c.statePath == "" {
		return
	}
	b, _ := json.Marshal(struct {
		Cursor uint32 `json:"cursor"`
		Skip   uint16 `json:"skip"`
	}{c.sync.Cursor, c.sync.Skip})
	if err := os.MkdirAll(filepath.Dir(c.statePath), 0o700); err == nil {
		os.WriteFile(c.statePath, b, 0o600)
	}
}

// run links to the node at addr and chats until the input ends, /quit, or
// ctx is done. It returns an error if the node refuses the link.
func (c *chat) run(ctx context.Context, addr string, in io.Reader, out io.Writer) error {
	c.out = out
	c.addr = addr
	c.unanswered = time.Now()
	c.loadState()
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	c.conn = conn
	defer conn.Close()
	c.printf("Linking to %s module %c as %s…  (/help for help)", addr, c.module, c.me)

	refused := make(chan struct{})
	go c.read(refused)

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	c.sendCONN()
	link := time.NewTicker(c.connRetry)
	defer link.Stop()
	tick := time.NewTicker(min(c.ackTimeout, c.pageQuiet) / 4)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.disconnect()
			return nil
		case <-refused:
			return fmt.Errorf("the node refused the link: check the module letter, and whether your callsign is on the node's allow list")
		case <-link.C:
			c.keepLinked()
		case now := <-tick.C:
			c.retry(now)
			c.syncTick(now)
		case line, ok := <-lines:
			if !ok {
				c.disconnect()
				return nil
			}
			if quit := c.handleInput(strings.TrimSpace(line)); quit {
				c.disconnect()
				return nil
			}
		}
	}
}

// keepLinked relinks when the node has gone quiet, and resends CONN until
// it answers.
func (c *chat) keepLinked() {
	c.mu.Lock()
	lost := c.linked && time.Since(c.lastPing) > c.pingTimeout
	if lost {
		c.linked = false
		c.unanswered = time.Now()
	}
	linked := c.linked
	warn := !linked && !c.unanswered.IsZero() && c.unanswered != c.warnedSince && time.Since(c.unanswered) >= c.noAnswer
	if warn {
		c.warnedSince = c.unanswered
	}
	c.mu.Unlock()
	if lost {
		c.printf("Link lost; relinking…")
	}
	if warn {
		c.printf("No answer from %s yet; still trying. Check that the station accepts internet clients on that port, and that your callsign is on its allow list.", c.addr)
	}
	if !linked {
		c.sendCONN()
	}
}

func (c *chat) read(refused chan<- struct{}) {
	buf := make([]byte, 2048)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. connection refused while the node is down
		}
		b := buf[:n]
		if len(b) < 4 {
			continue
		}
		switch string(b[:4]) {
		case m17.MagicACKN:
			c.mu.Lock()
			was := c.linked
			c.linked, c.lastPing = true, time.Now()
			c.unanswered = time.Time{}
			c.learnNode(b)
			node := c.node
			c.mu.Unlock()
			if !was {
				if node != 0 {
					c.printf("Linked to node %s.", node)
				} else {
					c.printf("Linked.")
				}
				c.startSync()
			}
		case m17.MagicNACK:
			close(refused)
			return
		case m17.MagicPING:
			c.mu.Lock()
			c.lastPing = time.Now()
			c.learnNode(b)
			c.mu.Unlock()
			c.sendControl(m17.MagicPONG)
		case m17.MagicDISC:
			c.mu.Lock()
			c.linked = false
			c.unanswered = time.Now()
			c.mu.Unlock()
			c.printf("The node closed the link; relinking…")
		case m17.MagicM17Packet:
			c.receivePacket(b)
		}
	}
}

// learnNode takes the node's callsign from an ACKN or PING that carries
// one, as qtcd's do. Callers hold c.mu.
func (c *chat) learnNode(b []byte) {
	if len(b) >= 10 {
		if a := envelope.AddressFromBytes(b[4:10]); a.IsStandard() {
			c.node = a
		}
	}
}

func (c *chat) receivePacket(b []byte) {
	if len(b) < 4+m17.LSFLen+3 {
		return
	}
	p := m17.NewPacketFromBytes(b[4:])
	if !p.LSF.CheckCRC() || !p.CheckCRC() {
		return
	}
	src := envelope.AddressFromBytes(p.LSF.Src[:])
	if p.Type == m17.PacketTypeSMS {
		// Not expected from a QTC node, but show it.
		text, _, _ := strings.Cut(string(p.Payload), "\x00")
		c.printf("%s %s: %s", stamp(), src, text)
		return
	}
	if byte(p.Type) != byte(envelope.TypeQTC) {
		return
	}
	e, err := envelope.Parse(append([]byte{byte(p.Type)}, p.Payload...))
	if err != nil {
		return // an unknown kind is ignored (client spec §2)
	}
	switch e.Kind() {
	case envelope.KindMSG:
		c.receiveMsg(e)
	case envelope.KindRCPT:
		c.receiveRcpt(e)
	case envelope.KindACK:
		a, _ := e.Ack()
		c.mu.Lock()
		for _, id := range a.IDs() {
			delete(c.pending, id)
		}
		c.mu.Unlock()
	case envelope.KindROOM:
		c.receiveRoom(e)
	case envelope.KindSYNC:
		c.receiveSync(e)
	}
}

// receiveMsg shows a message once, however often it comes, and
// acknowledges it every time: DELIVERED for one addressed to a callsign,
// ACK for a room message (client spec §4).
func (c *chat) receiveMsg(e *envelope.Envelope) {
	c.mu.Lock()
	dup := c.seen[e.ID()]
	c.seen[e.ID()] = true
	node := c.node
	if c.sync.active {
		c.sync.lastPkt = time.Now()
	}
	c.mu.Unlock()
	var ack *envelope.Envelope
	if e.Destination().IsRoom() {
		ack, _ = envelope.BuildAck([]envelope.ID{e.ID()})
	} else {
		ack, _ = envelope.BuildRcpt(c.meAddr, e.Source(), e.ID(), envelope.StatusDelivered, uint32(time.Now().Unix()), 0, "")
	}
	if ack != nil {
		c.sendPacket(node, ack)
	}
	if dup {
		return
	}
	m, _ := e.Msg()
	when := stamp()
	if ts := e.Timestamp(); ts != 0 && time.Since(time.Unix(int64(ts), 0)) > 12*time.Hour {
		when = time.Unix(int64(ts), 0).Format("Jan 2 15:04")
	} else if ts != 0 {
		when = time.Unix(int64(ts), 0).Format("15:04")
	}
	switch {
	case node != 0 && e.Source() == node:
		c.printf("%s * %s", when, m.Body())
	case e.Destination().IsRoom():
		c.printf("%s %s %s: %s", when, e.Destination(), e.Source(), m.Body())
	default:
		c.printf("%s %s: %s", when, e.Source(), m.Body())
	}
}

// receiveRcpt shows a receipt for something we sent.
func (c *chat) receiveRcpt(e *envelope.Envelope) {
	rc, _ := e.Rcpt()
	c.mu.Lock()
	text, ours := c.sentText[rc.MessageID()]
	if rc.Status() == envelope.StatusRejected {
		delete(c.pending, rc.MessageID())
	}
	if c.sync.active {
		c.sync.lastPkt = time.Now()
	}
	c.mu.Unlock()
	what := "your message"
	if ours {
		what = fmt.Sprintf("%q", text)
	}
	switch rc.Status() {
	case envelope.StatusDelivered:
		c.printf("%s * %s received %s", stamp(), e.Source(), what)
	case envelope.StatusTransmitted:
		c.printf("%s * %s sent on the air by %s", stamp(), what, e.Source())
	case envelope.StatusExpired:
		c.printf("%s * %s expired undelivered", stamp(), what)
	case envelope.StatusRejected:
		c.printf("%s * %s not sent: %s", stamp(), what, rc.Note())
	}
}

// receiveRoom shows the node's reply to a ROOM request.
func (c *chat) receiveRoom(e *envelope.Envelope) {
	r, _ := e.Room()
	c.mu.Lock()
	op := envelope.OpList
	if len(c.roomOp) > 0 {
		op, c.roomOp = c.roomOp[0], c.roomOp[1:]
	}
	c.mu.Unlock()
	names := func(as []envelope.Address) string {
		var out []string
		for _, a := range as {
			out = append(out, a.String())
		}
		return strings.Join(out, " ")
	}
	switch {
	case r.Op() == envelope.OpRefused:
		if n := names(r.Rooms()); n != "" {
			c.printf("%s * refused %s: %s", stamp(), n, r.Note())
		} else {
			c.printf("%s * refused: %s", stamp(), r.Note())
		}
	case op == envelope.OpList && len(r.Rooms()) == 0:
		c.printf("%s * no rooms", stamp())
	case op == envelope.OpList:
		c.printf("%s * rooms: %s", stamp(), names(r.Rooms()))
	case op == envelope.OpJoin:
		c.printf("%s * joined", stamp())
	default:
		c.printf("%s * left", stamp())
	}
}

// startSync asks for what we missed since the saved position.
func (c *chat) startSync() {
	c.mu.Lock()
	c.sync.active = true
	cursor, skip := c.sync.Cursor, c.sync.Skip
	c.mu.Unlock()
	c.requestPage(cursor, skip)
}

func (c *chat) requestPage(cursor uint32, skip uint16) {
	req, err := envelope.BuildSync(envelope.SyncRequest, 0, cursor, skip, 0, 0)
	if err != nil {
		return
	}
	c.mu.Lock()
	c.sync.requested = time.Now()
	c.sync.lastPkt = time.Time{}
	node := c.node
	c.mu.Unlock()
	c.sendPacket(node, req)
}

func (c *chat) receiveSync(e *envelope.Envelope) {
	y, _ := e.Sync()
	switch y.Op() {
	case envelope.SyncPage:
		c.mu.Lock()
		c.sync.active = true
		c.sync.requested = time.Time{}
		c.sync.remaining = y.Remaining()
		c.sync.next.cursor, c.sync.next.skip = y.Cursor(), y.Skip()
		c.sync.lastPkt = time.Now()
		c.mu.Unlock()
	case envelope.SyncNotify:
		c.mu.Lock()
		busy := c.sync.active
		c.mu.Unlock()
		if !busy {
			c.startSync()
		}
	case envelope.SyncRefused:
		c.mu.Lock()
		c.sync.active = false
		c.mu.Unlock()
		c.printf("%s * sync refused: %s", stamp(), y.Note())
	}
}

// syncTick moves a sync along: a page is done when nothing has arrived for
// pageQuiet; then the next page, or the end. A request with no PAGE is
// repeated.
func (c *chat) syncTick(now time.Time) {
	c.mu.Lock()
	st := c.sync
	c.mu.Unlock()
	if !st.active {
		return
	}
	switch {
	case !st.requested.IsZero():
		if now.Sub(st.requested) > c.pageQuiet {
			c.requestPage(st.Cursor, st.Skip) // no PAGE: ask again
		}
	case !st.lastPkt.IsZero() && now.Sub(st.lastPkt) > c.pageQuiet:
		c.mu.Lock()
		c.sync.Cursor, c.sync.Skip = st.next.cursor, st.next.skip
		c.saveState()
		done := st.remaining == 0
		if done {
			c.sync.active = false
		}
		c.mu.Unlock()
		if !done {
			c.requestPage(st.next.cursor, st.next.skip)
		}
	}
}

// retry resends our MSGs the node has not acknowledged, and gives up on
// them after ackRetries.
func (c *chat) retry(now time.Time) {
	var resend []*envelope.Envelope
	var failed []string
	c.mu.Lock()
	for id, o := range c.pending {
		if now.Before(o.next) {
			continue
		}
		if o.attempts > c.ackRetries {
			delete(c.pending, id)
			failed = append(failed, c.sentText[id])
			continue
		}
		o.attempts++
		o.next = now.Add(c.ackTimeout)
		resend = append(resend, o.e)
	}
	c.mu.Unlock()
	for _, e := range resend {
		c.sendPacket(e.Destination(), e)
	}
	for _, text := range failed {
		c.printf("%s * not sent, the node did not answer: %q", stamp(), text)
	}
}

// handleInput acts on one typed line and reports whether to quit.
func (c *chat) handleInput(line string) bool {
	switch {
	case line == "":
		return false
	case line == "/quit" || line == "/q":
		return true
	case line == "/help":
		c.printf("%s", chatHelp)
	case strings.HasPrefix(line, "/to "):
		to := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(line[4:])), "@")
		if !validTarget(to) {
			c.printf("%q is not a callsign or #ROOM", to)
			return false
		}
		c.mu.Lock()
		c.to = to
		c.mu.Unlock()
		c.printf("Plain text now goes to %s.", to)
	case strings.HasPrefix(line, "/"):
		c.roomCommand(line)
	case strings.HasPrefix(line, "#"):
		room, msg, _ := strings.Cut(line, " ")
		room = strings.ToUpper(room)
		if !validTarget(room) {
			c.printf("%q is not a room name", room)
			return false
		}
		c.mu.Lock()
		c.to = room
		c.mu.Unlock()
		c.send(room, strings.TrimSpace(msg))
	case strings.HasPrefix(line, "@"):
		call, msg, _ := strings.Cut(line[1:], " ")
		to := strings.ToUpper(call)
		if !validCallsign(to) {
			c.printf("%q is not a callsign", call)
			return false
		}
		c.mu.Lock()
		c.to = to
		c.mu.Unlock()
		if msg = strings.TrimSpace(msg); msg != "" {
			c.send(to, msg)
		} else {
			c.printf("Plain text now goes to %s.", to)
		}
	default:
		c.mu.Lock()
		to := c.to
		c.mu.Unlock()
		if to == "" {
			c.printf("Who to? Start with a callsign (@W1AW hello) or a room (#NET hello).")
			return false
		}
		c.send(to, line)
	}
	return false
}

func validCallsign(s string) bool {
	a, err := envelope.EncodeAddress(s)
	return err == nil && a.IsStandard() && !strings.ContainsAny(s, " ")
}

func validTarget(s string) bool {
	if name, ok := strings.CutPrefix(s, "#"); ok {
		_, err := envelope.RoomAddress(name)
		return err == nil
	}
	return validCallsign(s)
}

// roomCommand sends /join, /leave, or /rooms as a ROOM request.
func (c *chat) roomCommand(line string) {
	fields := strings.Fields(line)
	var op envelope.RoomOp
	switch strings.ToLower(fields[0]) {
	case "/join":
		op = envelope.OpJoin
	case "/leave":
		op = envelope.OpLeave
	case "/rooms":
		op = envelope.OpList
	default:
		c.printf("Unknown command %s; try /help.", fields[0])
		return
	}
	var rooms []envelope.Address
	for _, name := range fields[1:] {
		a, err := envelope.RoomAddress(strings.TrimPrefix(name, "#"))
		if err != nil {
			c.printf("%q is not a room name", name)
			return
		}
		rooms = append(rooms, a)
	}
	if op != envelope.OpList && len(rooms) == 0 {
		c.printf("Which room? For example: %s NET", fields[0])
		return
	}
	req, err := envelope.BuildRoom(op, uint32(time.Now().Unix()), rooms, "")
	if err != nil {
		c.printf("Cannot send: %v", err)
		return
	}
	c.mu.Lock()
	linked, node := c.linked, c.node
	if linked {
		c.roomOp = append(c.roomOp, op)
	}
	c.mu.Unlock()
	if !linked {
		c.printf("Not linked; not sent.")
		return
	}
	c.sendPacket(node, req)
}

// send sends a MSG to a callsign or "#ROOM", asking for receipts from a
// callsign, and keeps resending it until the node acknowledges it.
func (c *chat) send(to, text string) {
	if text == "" {
		return
	}
	if len(text) > maxText {
		c.printf("Message too long: %d bytes, the limit is %d.", len(text), maxText)
		return
	}
	var dst envelope.Address
	var err error
	var flags byte
	if name, ok := strings.CutPrefix(to, "#"); ok {
		dst, err = envelope.RoomAddress(name)
	} else {
		dst, err = envelope.EncodeAddress(to)
		flags = envelope.FlagRcptReq
	}
	if err != nil {
		c.printf("Cannot send to %s: %v", to, err)
		return
	}
	nonce, err := envelope.NewNonce()
	if err != nil {
		c.printf("Cannot send: %v", err)
		return
	}
	e, err := envelope.BuildMsg(c.meAddr, dst, uint32(time.Now().Unix()), envelope.TTLDefault, nonce, flags, text)
	if err != nil {
		c.printf("Cannot send: %v", err)
		return
	}
	c.mu.Lock()
	linked := c.linked
	if linked {
		c.pending[e.ID()] = &outgoing{e: e, attempts: 1, next: time.Now().Add(c.ackTimeout)}
		c.sentText[e.ID()] = text
		c.seen[e.ID()] = true // our own room posts are not shown back
	}
	c.mu.Unlock()
	if !linked {
		c.printf("Not linked; message not sent.")
		return
	}
	c.sendPacket(dst, e)
}

// sendPacket sends a QTC payload with the given LSF destination.
func (c *chat) sendPacket(dst envelope.Address, e *envelope.Envelope) {
	if dst == 0 {
		dst = envelope.Broadcast
	}
	b := e.Bytes()
	lsf, err := m17.NewLSF(c.me, c.me, m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	if err != nil {
		c.printf("Cannot send: %v", err)
		return
	}
	// Set the destination as raw bytes: m17's text encoding would read a
	// room's "#NAME" as an M17 hash address, not a QTC room.
	lsf.Dst = m17.EncodedCallsign(dst.Bytes())
	lsf.CalcCRC()
	p := m17.Packet{LSF: &lsf, Type: m17.PacketType(b[0]), Payload: b[1:]}
	p.CalcCRC()
	if _, err := c.conn.Write(append([]byte(m17.MagicM17Packet), p.ToBytes()...)); err != nil {
		c.printf("Send failed: %v", err)
	}
}

func (c *chat) sendCONN() {
	cs, err := m17.EncodeCallsign(c.me)
	if err != nil {
		return
	}
	b := append([]byte(m17.MagicCONN), cs[:]...)
	c.conn.Write(append(b, c.module))
}

func (c *chat) sendControl(magic string) {
	cs, err := m17.EncodeCallsign(c.me)
	if err != nil {
		return
	}
	c.conn.Write(append([]byte(magic), cs[:]...))
}

func (c *chat) disconnect() {
	c.sendControl(m17.MagicDISC)
}

func stamp() string { return time.Now().Format("15:04") }

func (c *chat) printf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, format+"\n", a...)
}
