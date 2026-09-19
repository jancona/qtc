package roost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jancona/pigeon/envelope"
)

// The client face (node protocol §10): the roost presents itself as one
// M17_inet reflector. Each module letter maps to an upstream reflector and
// module plus a mode. Native modules are a pure proxy; pigeon modules take
// messaging into Pigeon and never forward it.

// ModuleMode is a module's messaging mode.
type ModuleMode string

const (
	ModeNative ModuleMode = "native"
	ModePigeon ModuleMode = "pigeon"
)

// ModuleConfig maps one of this node's modules to an upstream.
type ModuleConfig struct {
	// Reflector is an upstream reflector name from the hosts file, or a
	// literal host:port.
	Reflector string
	// Module is the upstream module letter.
	Module byte
	Mode   ModuleMode
}

// InetConfig configures the client face.
type InetConfig struct {
	// Listen is the UDP address, e.g. "0.0.0.0:17000".
	Listen string
	// HostsFile resolves reflector names (M17Hosts.txt format). Optional if
	// every module uses a literal host:port.
	HostsFile string
	// Modules by letter.
	Modules map[byte]ModuleConfig
	// Gateways are address ranges whose clients are RF gateways (heard via
	// RF); other clients are internet clients. nil means the private and
	// loopback ranges.
	Gateways []*net.IPNet
	// SessionTimeout drops a client silent for this long; 0 means 60 s.
	SessionTimeout time.Duration
}

// inetCore is what the client face needs from the roost, kept small so the
// face can be tested against a stub.
type inetCore interface {
	inetHeard(device envelope.Address, via Via, now uint32) error
	inetSend(e *envelope.Envelope) error
	inetRoom(device envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error)
	inetCallsign() envelope.Address
	inetLocalRoom() envelope.Address
	inetDefaultTTL() uint16
	inetLog() *slog.Logger
}

// Roost implements inetCore.
func (r *Roost) inetHeard(d envelope.Address, via Via, now uint32) error { return r.Heard(d, via, now) }
func (r *Roost) inetSend(e *envelope.Envelope) error                     { return r.Send(e) }
func (r *Roost) inetRoom(d envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error) {
	return r.HandleRoom(d, req)
}
func (r *Roost) inetCallsign() envelope.Address  { return r.callsign }
func (r *Roost) inetLocalRoom() envelope.Address { return r.subs.LocalRoom() }
func (r *Roost) inetDefaultTTL() uint16          { return r.cfg.DefaultTTL }
func (r *Roost) inetLog() *slog.Logger           { return r.log }

type inetFace struct {
	core     inetCore
	cfg      InetConfig
	log      *slog.Logger
	conn     *net.UDPConn
	upstream map[byte]*net.UDPAddr // resolved per module

	mu       sync.Mutex
	sessions map[string]*inetSession           // by client address
	devices  map[envelope.Address]*inetSession // device -> pigeon-mode session that heard it
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

type inetSession struct {
	face     *inetFace
	client   *net.UDPAddr
	callsign envelope.Address
	module   byte
	mod      ModuleConfig
	pigeon   bool
	via      Via
	up       *net.UDPConn

	mu      sync.Mutex
	last    time.Time
	heardAt map[envelope.Address]time.Time
	closed  bool
}

const heardRateLimit = 5 * time.Second

func defaultGatewayNets() []*net.IPNet {
	var nets []*net.IPNet
	for _, c := range []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7", "fe80::/10"} {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}

// newInetFace resolves upstreams and binds the listen socket.
func newInetFace(core inetCore, cfg InetConfig) (*inetFace, error) {
	if cfg.SessionTimeout == 0 {
		cfg.SessionTimeout = 60 * time.Second
	}
	if cfg.Gateways == nil {
		cfg.Gateways = defaultGatewayNets()
	}
	if len(cfg.Modules) == 0 {
		return nil, errors.New("roost: inet: no modules configured")
	}
	var hosts map[string]reflectorHost
	f := &inetFace{
		core:     core,
		cfg:      cfg,
		log:      core.inetLog().With("face", "inet"),
		upstream: map[byte]*net.UDPAddr{},
		sessions: map[string]*inetSession{},
		devices:  map[envelope.Address]*inetSession{},
	}
	for letter, m := range cfg.Modules {
		if letter < 'A' || letter > 'Z' {
			return nil, fmt.Errorf("roost: inet: module %q is not A-Z", letter)
		}
		if m.Mode != ModeNative && m.Mode != ModePigeon {
			return nil, fmt.Errorf("roost: inet: module %c: mode must be native or pigeon", letter)
		}
		if m.Module < 'A' || m.Module > 'Z' {
			return nil, fmt.Errorf("roost: inet: module %c: upstream module %q is not A-Z", letter, m.Module)
		}
		addr := m.Reflector
		if !strings.Contains(addr, ":") {
			if hosts == nil {
				if cfg.HostsFile == "" {
					return nil, fmt.Errorf("roost: inet: module %c names reflector %q but no hosts file is configured", letter, m.Reflector)
				}
				var err error
				if hosts, err = loadHostsFile(cfg.HostsFile); err != nil {
					return nil, err
				}
			}
			h, ok := hosts[strings.ToUpper(m.Reflector)]
			if !ok {
				return nil, fmt.Errorf("roost: inet: module %c: reflector %q not in %s", letter, m.Reflector, cfg.HostsFile)
			}
			addr = h.Addr
		}
		ua, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("roost: inet: module %c: upstream %q: %w", letter, addr, err)
		}
		f.upstream[letter] = ua
	}
	la, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("roost: inet: listen %q: %w", cfg.Listen, err)
	}
	f.conn, err = net.ListenUDP("udp", la)
	if err != nil {
		return nil, fmt.Errorf("roost: inet: listen: %w", err)
	}
	return f, nil
}

// Addr is the bound listen address.
func (f *inetFace) Addr() *net.UDPAddr { return f.conn.LocalAddr().(*net.UDPAddr) }

// run serves until ctx ends, then closes every session.
func (f *inetFace) run(ctx context.Context) {
	f.ctx, f.cancel = context.WithCancel(ctx)
	f.wg.Add(1)
	go f.reaper()
	go func() {
		<-f.ctx.Done()
		f.conn.Close()
	}()
	buf := make([]byte, 2048)
	for {
		n, addr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			if f.ctx.Err() == nil {
				f.log.Warn("inet read", "err", err)
			}
			break
		}
		f.handleClient(append([]byte(nil), buf[:n]...), addr)
	}
	f.cancel()
	f.mu.Lock()
	for _, s := range f.sessions {
		s.close(false)
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// reaper drops sessions that have gone quiet.
func (f *inetFace) reaper() {
	defer f.wg.Done()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-t.C:
		}
		f.mu.Lock()
		for _, s := range f.sessions {
			s.mu.Lock()
			idle := time.Since(s.last)
			s.mu.Unlock()
			if idle > f.cfg.SessionTimeout {
				f.log.Info("client timed out", "client", s.client, "callsign", s.callsign)
				s.close(true)
			}
		}
		f.mu.Unlock()
	}
}

func (f *inetFace) via(ip net.IP) Via {
	for _, n := range f.cfg.Gateways {
		if n.Contains(ip) {
			return ViaRF
		}
	}
	return ViaInternet
}

func (f *inetFace) send(to *net.UDPAddr, b []byte) {
	if _, err := f.conn.WriteToUDP(b, to); err != nil {
		f.log.Debug("inet write", "to", to, "err", err)
	}
}

// handleClient dispatches one datagram from a gateway or client.
func (f *inetFace) handleClient(b []byte, addr *net.UDPAddr) {
	if len(b) < 4 {
		return
	}
	magic := string(b[:4])
	if magic == magicCONN || magic == magicLSTN {
		f.connect(b, addr)
		return
	}
	f.mu.Lock()
	s := f.sessions[addr.String()]
	f.mu.Unlock()
	if s == nil {
		f.log.Debug("datagram from unknown client", "client", addr, "magic", magic)
		return
	}
	s.touch()
	switch magic {
	case magicDISC:
		s.forwardUp(b)
		f.mu.Lock()
		s.close(false)
		f.mu.Unlock()
	case magicM17S:
		if _, src, ok := streamAddrs(b); ok && s.pigeon {
			s.heard(src)
		}
		s.forwardUp(b)
	case magicM17P:
		s.clientPacket(b)
	default: // PING, PONG, and anything new
		s.forwardUp(b)
	}
}

// connect handles CONN or LSTN: NACK for an unmapped module, otherwise open
// the upstream and relay the request with the upstream module letter.
func (f *inetFace) connect(b []byte, addr *net.UDPAddr) {
	if len(b) != 11 {
		f.send(addr, controlDatagram(magicNACK, f.core.inetCallsign()))
		return
	}
	module := b[10]
	mod, ok := f.cfg.Modules[module]
	if !ok {
		f.log.Info("NACK: unmapped module", "client", addr, "module", string(module))
		f.send(addr, controlDatagram(magicNACK, f.core.inetCallsign()))
		return
	}
	up, err := net.DialUDP("udp", nil, f.upstream[module])
	if err != nil {
		f.log.Warn("upstream dial failed", "module", string(module), "upstream", f.upstream[module], "err", err)
		f.send(addr, controlDatagram(magicNACK, f.core.inetCallsign()))
		return
	}
	s := &inetSession{
		face:     f,
		client:   addr,
		callsign: envelope.AddressFromBytes(b[4:10]),
		module:   module,
		mod:      mod,
		pigeon:   mod.Mode == ModePigeon,
		via:      f.via(addr.IP),
		up:       up,
		last:     time.Now(),
		heardAt:  map[envelope.Address]time.Time{},
	}
	f.mu.Lock()
	if old := f.sessions[addr.String()]; old != nil {
		old.close(true)
	}
	f.sessions[addr.String()] = s
	f.mu.Unlock()
	f.log.Info("client linking", "client", addr, "callsign", s.callsign, "module", string(module),
		"upstream", f.upstream[module], "upstream_module", string(mod.Module), "mode", mod.Mode)
	req := append([]byte(nil), b...)
	req[10] = mod.Module
	s.forwardUp(req)
	f.wg.Add(1)
	go s.readUpstream()
}

func (s *inetSession) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *inetSession) forwardUp(b []byte) {
	if _, err := s.up.Write(b); err != nil {
		s.face.log.Debug("upstream write", "client", s.client, "err", err)
	}
}

// close ends the session, sending DISC upstream if asked. Caller holds
// face.mu.
func (s *inetSession) close(disc bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if disc {
		s.forwardUp(controlDatagram(magicDISC, s.callsign))
	}
	s.up.Close()
	if s.face.sessions[s.client.String()] == s {
		delete(s.face.sessions, s.client.String())
	}
	for d, owner := range s.face.devices {
		if owner == s {
			delete(s.face.devices, d)
		}
	}
}

// heard publishes presence for a device seen on a pigeon module, at most
// once per heardRateLimit per device, and remembers the session for
// delivery.
func (s *inetSession) heard(device envelope.Address) {
	if !device.IsStandard() {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if last, ok := s.heardAt[device]; ok && now.Sub(last) < heardRateLimit {
		s.mu.Unlock()
		return
	}
	s.heardAt[device] = now
	s.mu.Unlock()
	s.face.mu.Lock()
	s.face.devices[device] = s
	s.face.mu.Unlock()
	if err := s.face.core.inetHeard(device, s.via, uint32(now.Unix())); err != nil {
		s.face.log.Debug("heard", "device", device, "err", err)
	}
}

// clientPacket handles an "M17P" datagram from the client.
func (s *inetSession) clientPacket(b []byte) {
	pf, err := parsePacketDatagram(b)
	if err != nil {
		s.face.log.Debug("unparseable packet from client; forwarding", "client", s.client, "err", err)
		s.forwardUp(b)
		return
	}
	if s.pigeon {
		s.heard(pf.src)
	}
	switch pf.typ {
	case envelope.TypeMSG, envelope.TypeRCPT, envelope.TypeROOM:
		s.ingestEnvelope(pf)
	case envelope.TypeSMS:
		if s.pigeon {
			s.ingestSMS(pf)
		} else {
			s.forwardUp(b)
		}
	default:
		s.forwardUp(b)
	}
}

// ingestEnvelope takes a native Pigeon packet into the roost.
func (s *inetSession) ingestEnvelope(pf packetFrame) {
	e, err := envelope.Parse(pf.payload)
	if err != nil {
		s.face.log.Info("bad envelope from client", "client", s.client, "err", err)
		return
	}
	if e.Type() == envelope.TypeROOM {
		reply, err := s.face.core.inetRoom(pf.src, e)
		if err != nil {
			s.face.log.Info("ROOM request rejected", "client", s.client, "err", err)
			return
		}
		s.face.send(s.client, buildPacketDatagram(pf.src, s.face.core.inetCallsign(), reply.Bytes()))
		return
	}
	if err := s.face.core.inetSend(e); err != nil {
		s.face.log.Info("envelope from client not sent", "client", s.client, "envelope", e, "err", err)
	}
}

// ingestSMS wraps a legacy SMS (envelope §6). An SMS to the node callsign
// is a room command when it begins with '/', otherwise a message to the
// local room (rooms §6).
func (s *inetSession) ingestSMS(pf packetFrame) {
	now := uint32(time.Now().Unix())
	node := s.face.core.inetCallsign()
	dst := pf.dst
	if dst == node {
		text := pf.payload[1:]
		if i := bytes.IndexByte(text, 0); i >= 0 {
			text = text[:i]
		}
		if t := strings.TrimSpace(string(text)); strings.HasPrefix(t, "/") {
			s.replySMS(pf.src, s.roomCommand(pf.src, t))
			return
		}
		dst = s.face.core.inetLocalRoom()
		if dst == envelope.AddressZero {
			s.replySMS(pf.src, "this node has no local room")
			return
		}
	}
	nonce, err := envelope.NewNonce()
	if err != nil {
		s.face.log.Error("nonce", "err", err)
		return
	}
	e, err := envelope.FromSMS(pf.payload, pf.src, dst, now, s.face.core.inetDefaultTTL(), nonce)
	if err != nil {
		s.face.log.Info("bad SMS from client", "client", s.client, "err", err)
		return
	}
	if err := s.face.core.inetSend(e); err != nil {
		s.face.log.Info("SMS not sent", "client", s.client, "envelope", e, "err", err)
		s.replySMS(pf.src, "not sent: "+err.Error())
	}
}

// roomCommand runs a legacy room command and returns the status line.
func (s *inetSession) roomCommand(device envelope.Address, text string) string {
	op, rooms, err := ParseRoomCommand(text)
	if err != nil {
		return "error: " + strings.TrimPrefix(err.Error(), "roost: ")
	}
	req, err := envelope.BuildRoom(op, uint32(time.Now().Unix()), rooms, "")
	if err != nil {
		return "error: " + err.Error()
	}
	reply, err := s.face.core.inetRoom(device, req)
	if err != nil {
		return "error: " + strings.TrimPrefix(err.Error(), "roost: ")
	}
	rr, _ := reply.Room()
	names := func(as []envelope.Address) string {
		var out []string
		for _, a := range as {
			out = append(out, strings.TrimPrefix(a.String(), "#"))
		}
		return strings.Join(out, " ")
	}
	switch {
	case rr.Op() == envelope.OpRefused:
		if n := names(rr.Rooms()); n != "" {
			return "refused " + n + ": " + rr.Note()
		}
		return "refused: " + rr.Note()
	case op == envelope.OpList:
		if len(rr.Rooms()) == 0 {
			return "no rooms"
		}
		return "rooms: " + names(rr.Rooms())
	case op == envelope.OpJoin:
		return "joined " + names(rooms)
	default:
		return "left " + names(rooms)
	}
}

// replySMS sends an SMS from the node callsign to a device on this session.
func (s *inetSession) replySMS(device envelope.Address, text string) {
	payload := append([]byte{byte(envelope.TypeSMS)}, text...)
	payload = append(payload, 0)
	s.face.send(s.client, buildPacketDatagram(device, s.face.core.inetCallsign(), payload))
}

// readUpstream relays reflector traffic to the client, dropping messaging
// packets on pigeon modules.
func (s *inetSession) readUpstream() {
	defer s.face.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, err := s.up.Read(buf)
		if err != nil {
			return
		}
		b := buf[:n]
		if len(b) < 4 {
			continue
		}
		if s.pigeon && string(b[:4]) == magicM17P {
			if pf, err := parsePacketDatagram(b); err == nil {
				switch pf.typ {
				case envelope.TypeSMS, envelope.TypeMSG, envelope.TypeRCPT, envelope.TypeROOM:
					s.face.log.Debug("dropping upstream messaging packet on pigeon module", "type", pf.typ, "src", pf.src)
					continue
				}
			}
		}
		s.face.send(s.client, b)
	}
}

// deliver sends a MSG to a device as SMS through the session that heard it.
// Receipts are dropped for legacy clients (envelope §6).
func (f *inetFace) deliver(device envelope.Address, e *envelope.Envelope) {
	f.mu.Lock()
	s := f.devices[device]
	f.mu.Unlock()
	if s == nil {
		return
	}
	if e.Type() != envelope.TypeMSG {
		f.log.Debug("receipt not delivered to legacy client", "device", device, "envelope", e)
		return
	}
	sms, err := envelope.ToSMS(e)
	if err != nil {
		return
	}
	f.log.Info("delivering SMS to client", "client", s.client, "device", device, "envelope", e)
	f.send(s.client, buildPacketDatagram(e.Destination(), e.Source(), sms))
}
