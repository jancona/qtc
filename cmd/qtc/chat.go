package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/qtc/envelope"
)

// qtc chat is a terminal client for a QTC node's client face (node protocol
// §10). It links to the node like a gateway or M17_inet client does and
// sends and receives M17 SMS, using the SMS conventions for rooms (rooms
// spec §6). It speaks M17_inet itself, with framing from the m17 package,
// because it needs the node's callsign from the node's ACKN and PING.

const chatHelp = `Type a message:
  W1AW: hello         send to a callsign
  #NET hello          post to a room
  hello               send to whoever you last wrote to
Commands:
  /join NET           join a room          /leave NET   leave a room
  /rooms              list your rooms      /to W1AW     set who plain text goes to
  /help               this help            /quit        disconnect and exit`

// maxSMSText keeps a message inside one M17 SMS packet.
const maxSMSText = 800

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
	me     string // our callsign, as sent in CONN and as the SMS source
	module byte

	// Link timing; tests shorten them.
	connRetry   time.Duration
	pingTimeout time.Duration

	mu       sync.Mutex
	out      io.Writer
	conn     *net.UDPConn
	node     envelope.Address // learned from the node's ACKN or PING
	linked   bool
	lastPing time.Time
	to       string // where plain text goes: a callsign or "#ROOM"
}

func newChat(callsign, module string) (*chat, error) {
	me := strings.ToUpper(strings.TrimSpace(callsign))
	if a, err := envelope.EncodeAddress(me); err != nil || !a.IsStandard() {
		return nil, fmt.Errorf("%q is not a callsign", callsign)
	}
	module = strings.ToUpper(module)
	if len(module) != 1 || module[0] < 'A' || module[0] > 'Z' {
		return nil, fmt.Errorf("module must be one letter A-Z")
	}
	return &chat{me: me, module: module[0], connRetry: 5 * time.Second, pingTimeout: 30 * time.Second}, nil
}

// run links to the node at addr and chats until the input ends, /quit, or
// ctx is done. It returns an error if the node refuses the link.
func (c *chat) run(ctx context.Context, addr string, in io.Reader, out io.Writer) error {
	c.out = out
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
	tick := time.NewTicker(c.connRetry)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			c.disconnect()
			return nil
		case <-refused:
			return fmt.Errorf("the node refused the link: check the module letter, and whether your callsign is on the node's allow list")
		case <-tick.C:
			c.keepLinked()
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
	}
	linked := c.linked
	c.mu.Unlock()
	if lost {
		c.printf("Link lost; relinking…")
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
			c.learnNode(b)
			node := c.node
			c.mu.Unlock()
			if !was {
				if node != 0 {
					c.printf("Linked to node %s.", node)
				} else {
					c.printf("Linked.")
				}
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
	if !p.LSF.CheckCRC() || !p.CheckCRC() || p.Type != m17.PacketTypeSMS {
		return
	}
	text := string(p.Payload)
	if i := strings.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	src := envelope.AddressFromBytes(p.LSF.Src[:])
	c.mu.Lock()
	fromNode := c.node != 0 && src == c.node
	c.mu.Unlock()
	stamp := time.Now().Format("15:04")
	switch {
	case fromNode:
		c.printf("%s * %s", stamp, text)
	case strings.HasPrefix(text, "#"):
		room, body, _ := strings.Cut(text, " ")
		c.printf("%s %s %s: %s", stamp, room, src, body)
	default:
		c.printf("%s %s: %s", stamp, src, text)
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
		to := strings.ToUpper(strings.TrimSpace(line[4:]))
		if !validTarget(to) {
			c.printf("%q is not a callsign or #ROOM", to)
			return false
		}
		c.mu.Lock()
		c.to = to
		c.mu.Unlock()
		c.printf("Plain text now goes to %s.", to)
	case strings.HasPrefix(line, "/"):
		// Room commands go to the node (rooms spec §6); it answers by SMS.
		c.sendToNode(line)
	case strings.HasPrefix(line, "#"):
		room, _, _ := strings.Cut(line, " ")
		c.mu.Lock()
		c.to = strings.ToUpper(room)
		c.mu.Unlock()
		c.sendToNode(line)
	default:
		if call, msg, ok := strings.Cut(line, ":"); ok && validCallsign(strings.ToUpper(strings.TrimSpace(call))) {
			to := strings.ToUpper(strings.TrimSpace(call))
			c.mu.Lock()
			c.to = to
			c.mu.Unlock()
			c.sendSMS(to, strings.TrimSpace(msg))
			return false
		}
		c.mu.Lock()
		to := c.to
		c.mu.Unlock()
		switch {
		case to == "":
			c.printf("Who to? Start with a callsign (W1AW: hello) or a room (#NET hello).")
		case strings.HasPrefix(to, "#"):
			c.sendToNode(to + " " + line)
		default:
			c.sendSMS(to, line)
		}
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

// sendToNode sends text to the node's own callsign: a room command, or a
// "#ROOM text" room message.
func (c *chat) sendToNode(text string) {
	c.mu.Lock()
	node := c.node
	c.mu.Unlock()
	if node == 0 {
		c.printf("Not linked to the node yet; try again in a moment.")
		return
	}
	c.sendSMS(node.String(), text)
}

func (c *chat) sendSMS(dst, text string) {
	if text == "" {
		return
	}
	if len(text) > maxSMSText {
		c.printf("Message too long: %d characters, the limit is %d.", len(text), maxSMSText)
		return
	}
	p, err := m17.NewPacket(dst, c.me, m17.PacketTypeSMS, append([]byte(text), 0))
	if err != nil {
		c.printf("Cannot send to %s: %v", dst, err)
		return
	}
	c.mu.Lock()
	linked := c.linked
	c.mu.Unlock()
	if !linked {
		c.printf("Not linked; message not sent.")
		return
	}
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

func (c *chat) printf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, format+"\n", a...)
}
