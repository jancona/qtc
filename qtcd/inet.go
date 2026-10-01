package qtcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
)

// The client face (node protocol §10): the station presents itself as one
// M17_inet reflector. Each module letter maps to an upstream reflector and
// module plus a mode. Native modules are a pure proxy; qtc modules take
// messaging into QTC and never forward it.

// ModuleMode is a module's messaging mode.
type ModuleMode string

const (
	ModeNative ModuleMode = "native"
	ModeQTC    ModuleMode = "qtc"
)

// ModuleConfig maps one of this node's modules to an upstream.
type ModuleConfig struct {
	// Reflector is an upstream reflector name from the hosts file, or a
	// literal host:port. Empty makes the module messaging-only: it has no
	// upstream, so voice and anything else not taken into QTC is dropped.
	// A messaging-only module must be a qtc module.
	Reflector string
	// Module is the upstream module letter; unused when Reflector is empty.
	Module byte
	Mode   ModuleMode
}

// InetConfig configures the client face.
type InetConfig struct {
	// Listen is the UDP address, e.g. "0.0.0.0:17000".
	Listen string
	// HostsFile resolves reflector names (M17Hosts.txt format), re-read
	// every HostsRefresh. Optional if every module uses a literal host:port.
	HostsFile string
	// HostsURL, used when HostsFile is empty, is where to download the hosts
	// file from, at start and every HostsRefresh (DefaultHostsURL, say).
	HostsURL string
	// HostsCache saves the last good download, so a start without network
	// still resolves names. Station sets it to DataDir/M17Hosts.txt.
	HostsCache string
	// HostsRefresh is how often names are resolved again; 0 means 24 h.
	HostsRefresh time.Duration
	// UserAgent is sent when downloading HostsURL.
	UserAgent string
	// Modules by letter.
	Modules map[byte]ModuleConfig
	// Gateways are address ranges whose clients are RF gateways (heard via
	// RF); other clients are internet clients. nil means the private and
	// loopback ranges.
	Gateways []*net.IPNet
	// SessionTimeout drops a client silent for this long; 0 means 60 s.
	SessionTimeout time.Duration
	// AllowCallsigns, when non-empty, limits internet clients (those outside
	// Gateways) to these base callsigns: a CONN from any other is NACKed,
	// and on a qtc module a packet or stream whose source is any other is
	// ignored. Gateways are not limited. This keeps strangers off an open
	// face; it does not verify that a client is who it claims to be.
	AllowCallsigns []string
}

// inetCore is what the client face needs from the station, kept small so the
// face can be tested against a stub.
type inetCore interface {
	inetHeard(device envelope.Address, via Via, now uint32) error
	inetRelinked(devices []envelope.Address)
	inetSend(e *envelope.Envelope) error
	inetRoom(device envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error)
	inetCallsign() envelope.Address
	inetLocalRoom() envelope.Address
	inetDefaultTTL() uint16
	inetLog() *slog.Logger

	// Native devices (docs/qtc-client.md).
	inetAcked(device envelope.Address, msg, rcpt *envelope.Envelope, fresh bool)
	inetLost(device envelope.Address, msg *envelope.Envelope, fresh bool)
	inetSync(device envelope.Address, req envelope.Sync, limit int) (page []*envelope.Envelope, cursor uint32, skip uint16, remaining int, err error)
	inetReachable(device envelope.Address) bool
	inetSubscribed(device, room envelope.Address) bool
}

// Station implements inetCore.
func (r *Station) inetHeard(d envelope.Address, via Via, now uint32) error {
	return r.Heard(d, via, now)
}
func (r *Station) inetRelinked(devices []envelope.Address) { r.Relinked(devices) }
func (r *Station) inetSend(e *envelope.Envelope) error     { return r.Send(e) }
func (r *Station) inetRoom(d envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error) {
	return r.HandleRoom(d, req)
}
func (r *Station) inetCallsign() envelope.Address  { return r.callsign }
func (r *Station) inetLocalRoom() envelope.Address { return r.subs.LocalRoom() }
func (r *Station) inetDefaultTTL() uint16          { return r.cfg.DefaultTTL }
func (r *Station) inetLog() *slog.Logger           { return r.log }

type inetFace struct {
	core  inetCore
	cfg   InetConfig
	log   *slog.Logger
	conn  *net.UDPConn
	allow map[envelope.Address]bool // base callsigns; nil allows all

	mu       sync.Mutex
	upstream map[byte]*net.UDPAddr             // per module with a reflector; missing until resolved
	sessions map[string]*inetSession           // by client address
	devices  map[envelope.Address]*inetSession // device -> qtc-mode session that heard it
	native   map[envelope.Address]bool         // devices whose latest packet was QTC, not SMS
	orphans  map[envelope.Address]orphan       // devices whose gateway link closed, until it relinks
	smsSeen  map[smsKey]time.Time              // recent SMS taken in, by content (repeatedSMS)
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
	qtcMode  bool
	via      Via
	up       *net.UDPConn // nil on a messaging-only module

	mu      sync.Mutex
	last    time.Time
	heardAt map[envelope.Address]time.Time
	closed  bool
	acked   bool          // upstream answered our CONN
	lastUp  time.Time     // last PING or PONG from upstream
	connReq []byte        // the CONN/LSTN as sent upstream, resent until acked
	connGap time.Duration // wait before the next unanswered CONN; grows to maxConnRetryInterval
	nextCon time.Time     // when the next CONN may be resent

	lastKeepHeard time.Time // internet clients: when keepHeard last heard the callsign

	nat nativeState // native devices on this link (native.go)
}

// Link keepalive timing. The node PINGs its client like any reflector, and
// relinks upstream when the reflector has been silent for upstreamSilence.
// An unanswered upstream CONN is resent after connRetryInterval, doubling
// each time up to maxConnRetryInterval, so a reflector that is down or
// not answering is not sent a CONN every few seconds indefinitely.
var (
	connRetryInterval    = 5 * time.Second
	maxConnRetryInterval = time.Minute
	clientPingInterval   = 3 * time.Second
	upstreamSilence      = 30 * time.Second
	// hostsRetry is how soon names are resolved again after a failed
	// download or an unresolved module, rather than waiting HostsRefresh.
	hostsRetry = 15 * time.Minute
)

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
		return nil, errors.New("qtcd: inet: no modules configured")
	}
	if cfg.HostsRefresh == 0 {
		cfg.HostsRefresh = 24 * time.Hour
	}
	f := &inetFace{
		core:     core,
		cfg:      cfg,
		log:      core.inetLog().With("face", "inet"),
		upstream: map[byte]*net.UDPAddr{},
		sessions: map[string]*inetSession{},
		devices:  map[envelope.Address]*inetSession{},
		native:   map[envelope.Address]bool{},
		orphans:  map[envelope.Address]orphan{},
		smsSeen:  map[smsKey]time.Time{},
	}
	for _, c := range cfg.AllowCallsigns {
		a, err := envelope.EncodeAddress(strings.TrimSpace(c))
		if err != nil || !a.IsStandard() {
			return nil, fmt.Errorf("qtcd: inet: allowed callsign %q: %w", c, errors.Join(err, ErrNotCallsign))
		}
		if f.allow == nil {
			f.allow = map[envelope.Address]bool{}
		}
		f.allow[a.Base()] = true
	}
	for letter, m := range cfg.Modules {
		if letter < 'A' || letter > 'Z' {
			return nil, fmt.Errorf("qtcd: inet: module %q is not A-Z", letter)
		}
		if m.Mode != ModeNative && m.Mode != ModeQTC {
			return nil, fmt.Errorf("qtcd: inet: module %c: mode must be native or qtc", letter)
		}
		if m.Reflector == "" {
			if m.Mode != ModeQTC {
				return nil, fmt.Errorf("qtcd: inet: module %c: a module with no reflector must be a qtc module", letter)
			}
			continue
		}
		if m.Module < 'A' || m.Module > 'Z' {
			return nil, fmt.Errorf("qtcd: inet: module %c: upstream module %q is not A-Z", letter, m.Module)
		}
		if !strings.Contains(m.Reflector, ":") && cfg.HostsFile == "" && cfg.HostsURL == "" {
			return nil, fmt.Errorf("qtcd: inet: module %c names reflector %q but no hosts file or URL is configured", letter, m.Reflector)
		}
	}
	// A name that does not resolve now is not fatal: its module refuses
	// links until a refresh resolves it (run, refreshHosts).
	var hosts map[string]reflectorHost
	if f.needsHosts() {
		var err error
		switch {
		case cfg.HostsFile != "":
			hosts, err = loadHostsFile(cfg.HostsFile)
		case cfg.HostsCache != "":
			// The download happens in run; until then, the last one.
			if hosts, err = loadHostsFile(cfg.HostsCache); errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			f.log.Warn("hosts file", "err", err)
		}
	}
	waiting := hosts == nil && cfg.HostsFile == "" && f.needsHosts()
	if waiting {
		f.log.Info("no hosts file yet; named reflectors resolve once it downloads", "url", cfg.HostsURL)
	}
	f.resolve(hosts, waiting)
	la, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("qtcd: inet: listen %q: %w", cfg.Listen, err)
	}
	f.conn, err = net.ListenUDP("udp", la)
	if err != nil {
		return nil, fmt.Errorf("qtcd: inet: listen: %w", err)
	}
	return f, nil
}

// Addr is the bound listen address.
func (f *inetFace) Addr() *net.UDPAddr { return f.conn.LocalAddr().(*net.UDPAddr) }

// run serves until ctx ends, then closes every session.
func (f *inetFace) run(ctx context.Context) {
	f.ctx, f.cancel = context.WithCancel(ctx)
	f.wg.Add(2)
	go f.reaper()
	go f.nativeLoop()
	if f.hasUpstreams() {
		f.wg.Add(1)
		go f.refreshHosts()
	}
	go func() {
		<-f.ctx.Done()
		f.conn.Close()
	}()
	buf := make([]byte, 2048)
	for {
		n, addr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			if f.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// Keep serving: a transient error must not take the face down
			// for every gateway and client.
			f.log.Warn("inet read", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		f.handleClient(append([]byte(nil), buf[:n]...), addr)
	}
	f.cancel()
	f.mu.Lock()
	for _, s := range f.sessions {
		s.close(true) // DISC upstream, or the reflector keeps the link and ignores our next CONN
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

// allowed reports whether a callsign heard over a link with the given via
// passes AllowCallsigns.
func (f *inetFace) allowed(a envelope.Address, via Via) bool {
	return f.allow == nil || via != ViaInternet || f.allow[a.Base()]
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
	if magic != magicM17S && magic != magicM17P {
		s.face.log.Debug("control from client", "client", addr, "magic", magic)
	}
	switch magic {
	case magicPONG:
		// Our PING answered; nothing to forward, the upstream link is ours.
	case magicPING:
		f.send(addr, controlDatagram(magicPONG, f.core.inetCallsign()))
	case magicDISC:
		s.forwardUp(b)
		f.mu.Lock()
		s.close(false)
		f.mu.Unlock()
	case magicM17S:
		// A relayed stream (reflector voice another gateway transmitted)
		// passes, but its source is not here.
		if _, src, relayed, ok := streamAddrs(b); ok && s.qtcMode && !relayed {
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
// the upstream and forward the request with the upstream module letter.
func (f *inetFace) connect(b []byte, addr *net.UDPAddr) {
	if len(b) != 11 {
		f.send(addr, controlDatagram(magicNACK, f.core.inetCallsign()))
		return
	}
	module := b[10]
	if call := envelope.AddressFromBytes(b[4:10]); !f.allowed(call, f.via(addr.IP)) {
		f.log.Info("NACK: callsign not allowed", "client", addr, "callsign", call)
		f.send(addr, controlDatagram(magicNACK, f.core.inetCallsign()))
		return
	}
	mod, ok := f.cfg.Modules[module]
	if !ok {
		f.log.Info("NACK: unmapped module", "client", addr, "module", string(module))
		f.send(addr, controlDatagram(magicNACK, f.core.inetCallsign()))
		return
	}
	var up *net.UDPConn
	var upAddr any = "none (messaging only)"
	if mod.Reflector != "" {
		f.mu.Lock()
		ua := f.upstream[module]
		f.mu.Unlock()
		// Not ready is not a refusal: NACK is for permanent refusals
		// (M17_inet), and a gateway does not retry after one. Leave the
		// CONN unanswered; the gateway resends it with backoff, and links
		// once the upstream resolves.
		if ua == nil {
			f.log.Warn("not answering link: upstream reflector not resolved yet", "client", addr, "module", string(module), "reflector", mod.Reflector)
			return
		}
		var err error
		if up, err = net.DialUDP("udp", nil, ua); err != nil {
			f.log.Warn("not answering link: upstream dial failed", "client", addr, "module", string(module), "upstream", ua, "err", err)
			return
		}
		upAddr = ua
	}
	s := &inetSession{
		face:     f,
		client:   addr,
		callsign: envelope.AddressFromBytes(b[4:10]),
		module:   module,
		mod:      mod,
		qtcMode:  mod.Mode == ModeQTC,
		via:      f.via(addr.IP),
		up:       up,
		last:     time.Now(),
		heardAt:  map[envelope.Address]time.Time{},
		nat:      newNativeState(),
	}
	f.mu.Lock()
	if old := f.sessions[addr.String()]; old != nil {
		old.close(true)
	}
	f.sessions[addr.String()] = s
	relinked := f.adoptOrphans(s)
	f.mu.Unlock()
	if len(relinked) > 0 {
		f.log.Info("gateway relinked; radios reattached", "client", addr, "callsign", s.callsign, "devices", relinked)
		f.core.inetRelinked(relinked)
	}
	if up != nil {
		f.log.Info("client linking", "client", addr, "callsign", s.callsign, "module", string(module),
			"upstream", upAddr, "upstream_module", string(mod.Module), "mode", mod.Mode)
	} else {
		f.log.Info("client linking", "client", addr, "callsign", s.callsign, "module", string(module),
			"upstream", upAddr, "mode", mod.Mode)
	}
	// The client is linked to this node, not to the upstream: answer it now
	// and manage the upstream link in the background.
	f.send(addr, controlDatagram(magicACKN, f.core.inetCallsign()))
	if up != nil {
		req := append([]byte(nil), b...)
		req[10] = mod.Module
		s.connReq = req
		s.forwardUp(req)
		f.wg.Add(2)
		go s.readUpstream()
		go s.retryConn()
	}
	f.wg.Add(1)
	go s.pingClient()
	s.keepHeard()
}

// pingClient keeps the client's link alive the way a reflector does.
func (s *inetSession) pingClient() {
	defer s.face.wg.Done()
	t := time.NewTicker(clientPingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.face.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		s.face.send(s.client, controlDatagram(magicPING, s.face.core.inetCallsign()))
	}
}

// retryConn resends the upstream CONN until the reflector accepts or the
// session ends. Reflectors drop CONNs silently, or NACK them, while a stale
// link for the same callsign is still timing out.
func (s *inetSession) retryConn() {
	defer s.face.wg.Done()
	t := time.NewTicker(connRetryInterval)
	defer t.Stop()
	for {
		select {
		case <-s.face.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		now := time.Now()
		if s.acked && now.Sub(s.lastUp) > upstreamSilence {
			s.acked = false
			s.connGap = 0
			s.nextCon = now
			s.face.log.Warn("upstream silent; relinking", "client", s.client, "upstream", s.up.RemoteAddr())
		}
		due := !s.acked && !now.Before(s.nextCon)
		if due {
			if s.connGap == 0 {
				s.connGap = connRetryInterval
			} else {
				s.connGap = min(2*s.connGap, maxConnRetryInterval)
			}
			s.nextCon = now.Add(s.connGap)
		}
		s.mu.Unlock()
		if !due {
			continue
		}
		s.face.log.Debug("resending upstream CONN", "client", s.client, "upstream", s.up.RemoteAddr())
		s.forwardUp(s.connReq)
	}
}

func (s *inetSession) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
	s.keepHeard()
}

// internetHeardInterval is how often a linked internet client's callsign is
// heard again while its link stays up.
const internetHeardInterval = time.Minute

// keepHeard hears an internet client's own callsign when it links and then
// while its link stays up, on a qtc module. Someone at a client is present
// while it is connected, so its messages are in reach (node protocol §7.4)
// without it having to send. A gateway's link says nothing about whether a
// radio is listening, so gateways are heard only by what they carry.
func (s *inetSession) keepHeard() {
	if !s.qtcMode || s.via != ViaInternet {
		return
	}
	s.mu.Lock()
	due := time.Since(s.lastKeepHeard) >= internetHeardInterval
	if due {
		s.lastKeepHeard = time.Now()
	}
	s.mu.Unlock()
	if due {
		s.heard(s.callsign)
	}
}

// forwardUp sends to the upstream reflector; on a messaging-only module
// there is none, and the datagram is dropped.
func (s *inetSession) forwardUp(b []byte) {
	if s.up == nil {
		return
	}
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
	if s.up != nil {
		s.up.Close()
	}
	if s.face.sessions[s.client.String()] == s {
		delete(s.face.sessions, s.client.String())
	}
	for d, owner := range s.face.devices {
		if owner == s {
			delete(s.face.devices, d)
			if s.via == ViaRF {
				s.face.orphans[d] = orphan{gateway: s.callsign, module: s.module, at: time.Now()}
			}
		}
	}
}

// orphan is a device heard through a gateway whose link has closed. A
// gateway restarting (a config change, an upgrade, a reboot) relinks under
// the same callsign and module; its radios have not gone anywhere.
type orphan struct {
	gateway envelope.Address
	module  byte
	at      time.Time
}

// orphanTTL bounds how long a closed gateway link's devices are remembered.
const orphanTTL = 24 * time.Hour

// adoptOrphans reattaches to a new gateway session the devices its
// previous link had heard, and returns them. Callers hold f.mu.
func (f *inetFace) adoptOrphans(s *inetSession) []envelope.Address {
	var out []envelope.Address
	for d, o := range f.orphans {
		switch {
		case time.Since(o.at) > orphanTTL:
			delete(f.orphans, d)
		case s.via == ViaRF && s.qtcMode && o.gateway == s.callsign && o.module == s.module:
			delete(f.orphans, d)
			f.devices[d] = s
			out = append(out, d)
		}
	}
	return out
}

// heard publishes presence for a device seen on a qtc module, at most
// once per heardRateLimit per device, and remembers the session for
// delivery.
func (s *inetSession) heard(device envelope.Address) {
	if !device.IsStandard() || !s.face.allowed(device, s.via) {
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
	delete(s.face.orphans, device)
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
	if s.qtcMode {
		if pf.relayed && (pf.typ == envelope.TypeSMS || pf.typ == envelope.TypeQTC) {
			// Another gateway transmitted this from the network, and this
			// one heard it. Taking it in would send it round again, as a
			// new message for an SMS, and its source is not here.
			s.face.log.Debug("relayed packet heard on RF; dropped", "client", s.client, "src", pf.src, "dst", pf.dst, "type", pf.typ)
			return
		}
		if !s.face.allowed(pf.src, s.via) {
			s.face.log.Info("packet from callsign not allowed; dropped", "client", s.client, "src", pf.src)
			return
		}
		s.heard(pf.src)
	}
	switch pf.typ {
	case envelope.TypeQTC:
		s.ingestEnvelope(pf)
	case envelope.TypeSMS:
		if s.qtcMode {
			s.face.setNative(pf.src, false)
			s.ingestSMS(pf)
		} else {
			s.forwardUp(b)
		}
	default:
		s.forwardUp(b)
	}
}

// ingestEnvelope takes a QTC packet into the station. On a qtc module its
// sender is a native device (client spec §2), with acknowledgement, sync,
// and fetch; on a native module MSG, RCPT, and ROOM are still taken in,
// since no reflector understands them.
func (s *inetSession) ingestEnvelope(pf packetFrame) {
	e, err := envelope.Parse(pf.payload)
	if err != nil {
		s.face.log.Info("bad QTC packet from client", "client", s.client, "err", err)
		return
	}
	if s.qtcMode && pf.src.IsStandard() {
		s.face.setNative(pf.src, true)
		if e.Kind() != envelope.KindROOM {
			s.ingestNative(pf, e)
			return
		}
	}
	if e.Kind() == envelope.KindROOM {
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
	dst := pf.dst
	if isNodeAddress(dst, s.face.core.inetCallsign()) {
		text := pf.payload[1:]
		if i := bytes.IndexByte(text, 0); i >= 0 {
			text = text[:i]
		}
		t := strings.TrimSpace(string(text))
		switch {
		case strings.HasPrefix(t, "/"):
			s.replySMS(pf.src, s.roomCommand(pf.src, t))
			return
		case strings.HasPrefix(t, "#"):
			// "#ROOM text": a message to a named room (rooms spec §6).
			name, rest, _ := strings.Cut(t, " ")
			room, err := envelope.RoomAddress(strings.TrimPrefix(name, "#"))
			if err != nil {
				s.replySMS(pf.src, "error: bad room name "+strings.TrimPrefix(name, "#"))
				return
			}
			dst = room
			pf.payload = append([]byte{byte(envelope.TypeSMS)}, append([]byte(strings.TrimSpace(rest)), 0)...)
		default:
			dst = s.face.core.inetLocalRoom()
			if dst == envelope.AddressZero {
				s.replySMS(pf.src, "this node has no local room")
				return
			}
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
	if s.face.repeatedSMS(e) {
		s.face.log.Info("repeated SMS dropped", "client", s.client, "envelope", e)
		return
	}
	if err := s.face.core.inetSend(e); err != nil {
		s.face.log.Info("SMS not sent", "client", s.client, "envelope", e, "err", err)
		s.replySMS(pf.src, "not sent: "+err.Error())
	}
}

// smsRepeatWindow is how long an SMS's content is remembered: an identical
// one (same source, destination, and text) within it is not taken in again.
var smsRepeatWindow = 5 * time.Minute

type smsKey struct {
	src, dst envelope.Address
	body     string
}

// repeatedSMS reports whether an identical SMS was taken in within
// smsRepeatWindow, and remembers this one either way. An SMS has no ID of
// its own, so this is what stops two hotspots that hear each other from
// passing one message back and forth as ever-new ones when the relayed
// copies carry no mark (see relayed); each sighting renews the window. The
// cost is that a radio sending the same text to the same place twice in a
// few minutes gets it through once.
func (f *inetFace) repeatedSMS(e *envelope.Envelope) bool {
	m, _ := e.Msg()
	k := smsKey{src: e.Source(), dst: e.Destination(), body: m.Body()}
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	last, seen := f.smsSeen[k]
	f.smsSeen[k] = now
	if len(f.smsSeen) > 1000 {
		for key, at := range f.smsSeen {
			if now.Sub(at) > smsRepeatWindow {
				delete(f.smsSeen, key)
			}
		}
	}
	return seen && now.Sub(last) < smsRepeatWindow
}

// isNodeAddress reports whether a client addressed the node itself. Runs of
// spaces are not significant: the module convention pads "N1ADJ  M" to put
// the letter in the ninth position, but radio UIs collapse or drop the
// padding, so "N1ADJ M" must reach the node too (node protocol §10).
func isNodeAddress(dst, node envelope.Address) bool {
	if dst == node {
		return true
	}
	d, err1 := dst.Text()
	n, err2 := node.Text()
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.Join(strings.Fields(d), " ") == strings.Join(strings.Fields(n), " ")
}

// roomCommand runs a legacy room command and returns the status line.
func (s *inetSession) roomCommand(device envelope.Address, text string) string {
	op, rooms, err := ParseRoomCommand(text)
	if err != nil {
		if errors.Is(err, ErrBadRoomName) {
			return "error: bad room name"
		}
		if errors.Is(err, ErrUnknownCmd) {
			return "error: unknown command; try /join /leave /rooms"
		}
		return "error: " + strings.TrimPrefix(err.Error(), "qtcd: ")
	}
	req, err := envelope.BuildRoom(op, uint32(time.Now().Unix()), rooms, "")
	if err != nil {
		return "error: " + err.Error()
	}
	reply, err := s.face.core.inetRoom(device, req)
	if err != nil {
		return "error: " + strings.TrimPrefix(err.Error(), "qtcd: ")
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

// readUpstream forwards reflector traffic to the client, dropping messaging
// packets on qtc modules.
func (s *inetSession) readUpstream() {
	defer s.face.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, err := s.up.Read(buf)
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			// A UDP socket reports an ICMP error (the reflector restarting,
			// a network blip) as a failed read. Keep reading: returning here
			// would leave the session deaf to the reflector's ACKN and PINGs
			// while retryConn kept sending CONN.
			s.face.log.Debug("upstream read error; still reading", "client", s.client, "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		b := buf[:n]
		if len(b) < 4 {
			continue
		}
		if m := string(b[:4]); m != magicM17S && m != magicM17P {
			s.face.log.Debug("control from upstream", "client", s.client, "magic", m)
		}
		switch string(b[:4]) {
		case magicPING:
			// Answer as the client would, and do not forward: the client's
			// link is kept alive by the node's own PINGs.
			s.forwardUp(controlDatagram(magicPONG, s.callsign))
			s.mu.Lock()
			s.lastUp = time.Now()
			s.mu.Unlock()
			continue
		case magicPONG:
			s.mu.Lock()
			s.lastUp = time.Now()
			s.mu.Unlock()
			continue
		case magicACKN:
			s.mu.Lock()
			first := !s.acked
			s.acked = true
			s.lastUp = time.Now()
			s.connGap = 0
			s.mu.Unlock()
			if first {
				s.face.log.Info("upstream linked", "client", s.client, "upstream", s.up.RemoteAddr())
			}
			continue // the client was answered when it linked to us
		case magicNACK:
			s.face.log.Warn("upstream refused the link; will retry", "client", s.client, "upstream", s.up.RemoteAddr())
			continue
		}
		if s.qtcMode && string(b[:4]) == magicM17P {
			if pf, err := parsePacketDatagram(b); err == nil {
				switch pf.typ {
				case envelope.TypeSMS, envelope.TypeQTC:
					s.face.log.Debug("dropping upstream messaging packet on qtc module", "type", pf.typ, "src", pf.src)
					continue
				}
			}
		}
		s.face.send(s.client, b)
	}
}

// deliver sends a MSG or RCPT to a device through the session that heard
// it: to a native device as a QTC payload (native.go), to a legacy one as
// SMS, with receipts dropped (envelope §6).
func (f *inetFace) deliver(device envelope.Address, e *envelope.Envelope) deliverResult {
	f.mu.Lock()
	s := f.devices[device]
	native := f.native[device]
	f.mu.Unlock()
	if s == nil {
		f.log.Debug("no link to device", "device", device)
		return deliverNone
	}
	if native {
		return s.deliverNative(device, e)
	}
	if e.Kind() != envelope.KindMSG {
		f.log.Debug("receipt not delivered to legacy client", "device", device, "envelope", e)
		return deliverSent
	}
	sms, err := envelope.ToSMS(e)
	if err != nil {
		f.log.Warn("cannot send as SMS", "device", device, "envelope", e, "err", err)
		return deliverSent
	}
	if name, ok := e.Destination().RoomName(); ok {
		// A legacy radio cannot show an Extended address, so a room message
		// names the room in the text (rooms spec §6).
		body := append([]byte("#"+name+" "), sms[1:]...)
		sms = append([]byte{byte(envelope.TypeSMS)}, body...)
	}
	// Addressed to the device, suffix and all, not to the envelope's
	// destination: a radio shows only SMS to its own callsign, so one sent
	// to "N1ADJ" never appears on "N1ADJ 8" (envelope §6).
	f.log.Info("delivering SMS to client", "client", s.client, "device", device, "envelope", e)
	f.send(s.client, buildPacketDatagram(device, e.Source(), sms))
	return deliverSent
}
