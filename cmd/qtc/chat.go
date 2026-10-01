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
	"github.com/jancona/qtc/client"
	"github.com/jancona/qtc/envelope"
	"golang.org/x/term"
)

// qtc chat is a terminal client for a QTC node's client face (node protocol
// §10). It links to the node like a gateway or M17_inet client does and is
// a native client (docs/qtc-client.md): it sends and receives the QTC
// packet type, acknowledges what it receives, resends what the node has not
// acknowledged, and syncs what it missed each time it links. It speaks
// M17_inet itself, with framing from the m17 package, because it needs the
// node's callsign from the node's ACKN and PING.

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
	c.statePath = client.DefaultStatePath(c.meAddr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var out io.Writer = os.Stdout
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) && term.IsTerminal(int(os.Stdout.Fd())) {
		// Edit the line being typed (arrow keys, history), and keep it
		// intact when a message prints while typing. Ctrl-C or Ctrl-D
		// ends input, which quits.
		old, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, old)
		t := term.NewTerminal(struct {
			io.Reader
			io.Writer
		}{os.Stdin, os.Stdout}, "> ")
		c.readLine, out = t.ReadLine, t
	}
	return c.run(ctx, addr, os.Stdin, out)
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

	// Link and protocol timing; tests shorten them.
	connRetry   time.Duration
	pingTimeout time.Duration
	noAnswer    time.Duration // unanswered this long, say so once
	ackTimeout  time.Duration
	ackRetries  int
	pageQuiet   time.Duration

	// statePath keeps the sync position between runs; "" keeps nothing.
	statePath string

	addr        string
	unanswered  time.Time // when we started waiting for an answer; zero once linked
	warnedSince time.Time // the wait already warned about

	sess *client.Session
	term *client.Terminal

	// readLine reads a typed line; nil reads lines from run's input.
	readLine func() (string, error)

	mu       sync.Mutex
	out      io.Writer
	conn     *net.UDPConn
	node     envelope.Address // learned from the node's ACKN or PING
	linked   bool
	lastPing time.Time
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
	c := &chat{me: me, meAddr: a, module: module[0], connRetry: 5 * time.Second, pingTimeout: 30 * time.Second, noAnswer: 10 * time.Second}
	c.term = &client.Terminal{Out: func(line string) { c.printf("%s", line) }}
	return c, nil
}

// handleInput acts on one typed line and reports whether to quit.
func (c *chat) handleInput(line string) bool { return c.term.Input(line) }

// run links to the node at addr and chats until the input ends, /quit, or
// ctx is done. It returns an error if the node refuses the link.
func (c *chat) run(ctx context.Context, addr string, in io.Reader, out io.Writer) error {
	c.out = out
	c.addr = addr
	c.unanswered = time.Now()
	c.sess = client.New(client.Config{
		Me: c.meAddr, AckTimeout: c.ackTimeout, AckRetries: c.ackRetries, PageQuiet: c.pageQuiet,
		StatePath: c.statePath, Send: c.sendPacket, Event: c.term.Show,
	})
	c.term.Sess = c.sess
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

	readLine := c.readLine
	if readLine == nil {
		sc := bufio.NewScanner(in)
		readLine = func() (string, error) {
			if sc.Scan() {
				return sc.Text(), nil
			}
			return "", io.EOF
		}
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		for {
			line, err := readLine()
			if err != nil {
				return
			}
			lines <- line
		}
	}()

	c.sendCONN()
	link := time.NewTicker(c.connRetry)
	defer link.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
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
			c.sess.Tick(now)
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
		c.sess.Unlinked()
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
				c.sess.Linked()
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
			c.sess.Unlinked()
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
			c.sess.SetNode(a)
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
		c.printf("%s %s: %s", time.Now().Format("15:04"), src, text)
		return
	}
	if byte(p.Type) != byte(envelope.TypeQTC) {
		return
	}
	e, err := envelope.Parse(append([]byte{byte(p.Type)}, p.Payload...))
	if err != nil {
		return // an unknown kind is ignored (client spec §2)
	}
	c.sess.Receive(envelope.AddressFromBytes(p.LSF.Dst[:]), src, e)
}

// sendPacket sends a QTC payload with the given LSF destination.
func (c *chat) sendPacket(dst envelope.Address, e *envelope.Envelope) {
	p, err := qtcPacket(dst, c.meAddr, e)
	if err != nil {
		c.printf("Cannot send: %v", err)
		return
	}
	if _, err := c.conn.Write(append([]byte(m17.MagicM17Packet), p.ToBytes()...)); err != nil {
		c.printf("Send failed: %v", err)
	}
}

// qtcPacket frames a QTC payload as an M17 packet. The addresses are set
// as raw bytes: m17's text encoding would read a room's "#NAME" as an M17
// hash address, not a QTC room.
func qtcPacket(dst, src envelope.Address, e *envelope.Envelope) (m17.Packet, error) {
	lsf, err := m17.NewLSF("N0CALL", "N0CALL", m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	if err != nil {
		return m17.Packet{}, err
	}
	lsf.Dst = m17.EncodedCallsign(dst.Bytes())
	lsf.Src = m17.EncodedCallsign(src.Bytes())
	lsf.CalcCRC()
	b := e.Bytes()
	p := m17.Packet{LSF: &lsf, Type: m17.PacketType(b[0]), Payload: b[1:]}
	p.CalcCRC()
	return p, nil
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
	line := fmt.Sprintf(format, a...)
	if line == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.out, line)
}
