package qtcd

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Capabilities are the node card's capability flags (node protocol §2).
type Capabilities uint8

const (
	CapPublic  Capabilities = 1
	CapRelay   Capabilities = 2
	CapMailbox Capabilities = 4
	CapClients Capabilities = 8
)

// Has reports whether every flag in c is set.
func (caps Capabilities) Has(c Capabilities) bool { return caps&c == c }

// Via says how a device was heard (node protocol §3).
type Via int

const (
	ViaRF       Via = 1
	ViaLocal    Via = 2
	ViaInternet Via = 3
)

// Config configures a Station. Zero durations take the node protocol §11
// defaults.
type Config struct {
	// Callsign is the node callsign, used whole ("K1XYZ  R"). Its base
	// callsign names the local room.
	Callsign string
	// KeyFile holds the node's ECDSA P-256 key in PKCS #8 PEM. It is
	// generated if missing. Empty uses an ephemeral key (tests).
	KeyFile string
	// ListenAddrs are libp2p multiaddrs to listen on. Public stations should
	// use a fixed port; others may use port 0.
	ListenAddrs []string
	// Bootstrap are multiaddrs (with /p2p/<ID>) of public stations to connect
	// to at start. They also serve as static circuit relays for a node behind NAT.
	Bootstrap []string
	// Caps is the node card's capability set. CapMailbox runs a store server.
	Caps Capabilities
	// Software is the node card's software string.
	Software string
	// EnableDHT turns on Kademlia peer discovery. Off is for in-process tests.
	EnableDHT bool

	// MailboxMembers is the spike's static mailbox: the peer IDs used as the
	// mailbox set for every callsign, in place of DHT mailbox records.
	MailboxMembers []string

	// Rooms lists the rooms this node carries; empty carries all.
	Rooms []string

	// Devices are local devices this node hears at start, as if from a
	// directly connected client. It stands in for the M17_inet face during
	// the spike.
	Devices []string

	// Deliver receives every envelope delivered to a local device. nil logs.
	Deliver func(device envelope.Address, e *envelope.Envelope)

	// Inet configures the M17_inet client face (node protocol §10). nil
	// disables it.
	Inet *InetConfig

	// EchoRoomMessages delivers a room message back to the device that
	// sent it. Off by default: useful for testing, noise on the air.
	EchoRoomMessages bool

	PresenceInterval time.Duration // default 5 min
	SweepInterval    time.Duration // default 1 h
	ReplayWindow     time.Duration // default 3 h
	ActiveWindow     time.Duration // default 24 h
	MetricsInterval  time.Duration // 0 disables the metrics log
	DefaultTTL       uint16        // minutes; default 7 days

	Log *slog.Logger
}

func (c *Config) defaults() {
	if c.PresenceInterval == 0 {
		c.PresenceInterval = 5 * time.Minute
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = time.Hour
	}
	if c.ReplayWindow == 0 {
		c.ReplayWindow = 3 * time.Hour
	}
	if c.ActiveWindow == 0 {
		c.ActiveWindow = 24 * time.Hour
	}
	if c.DefaultTTL == 0 {
		c.DefaultTTL = store.DefaultTTLMinutes
	}
	if c.Software == "" {
		c.Software = "qtcd/0"
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Station is a QTC node.
type Station struct {
	cfg      Config
	log      *slog.Logger
	callsign envelope.Address
	key      *ecdsa.PrivateKey

	host host.Host
	dht  *dht.IpfsDHT
	ps   *pubsub.PubSub

	presence *presence
	subs     *Subscriptions
	rooms    *roomTopics
	mailbox  *mailboxSet
	dialer   *peerDialer
	inet     *inetFace
	mem      *store.MemStore
	server   *store.Server

	mu        sync.Mutex
	homed     map[envelope.Address]*homed // by base callsign
	delivered map[deliveryKey]struct{}
	queued    map[envelope.ID]struct{} // QUEUED already issued

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type deliveryKey struct {
	id     envelope.ID
	device envelope.Address
}

// New builds a Station from cfg without starting any network activity.
func New(cfg Config) (*Station, error) {
	cfg.defaults()
	callsign, err := envelope.EncodeAddress(cfg.Callsign)
	if err != nil || !callsign.IsStandard() {
		return nil, fmt.Errorf("qtcd: node callsign %q: %w", cfg.Callsign, errors.Join(err, ErrNotCallsign))
	}
	key, err := loadOrCreateKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	carried := map[envelope.Address]bool{}
	for _, name := range cfg.Rooms {
		a, err := envelope.RoomAddress(name)
		if err != nil {
			return nil, fmt.Errorf("qtcd: room %q: %w", name, err)
		}
		carried[a] = true
	}
	var carries func(envelope.Address) bool
	if len(carried) > 0 {
		carries = func(a envelope.Address) bool { return carried[a] }
	}
	subs, err := NewSubscriptions(SubscriptionsConfig{NodeCallsign: cfg.Callsign, Carries: carries})
	if err != nil {
		return nil, err
	}
	r := &Station{
		cfg:       cfg,
		log:       cfg.Log.With("node", cfg.Callsign),
		callsign:  callsign,
		key:       key,
		subs:      subs,
		homed:     map[envelope.Address]*homed{},
		delivered: map[deliveryKey]struct{}{},
		queued:    map[envelope.ID]struct{}{},
	}
	if cfg.Deliver == nil {
		r.cfg.Deliver = func(device envelope.Address, e *envelope.Envelope) {
			r.log.Info("deliver", "device", device, "envelope", e)
		}
	}
	return r, nil
}

// Start brings up the libp2p host, pubsub, the store server if this node
// has CapMailbox, presence, and the configured local devices.
func (r *Station) Start(ctx context.Context) error {
	r.ctx, r.cancel = context.WithCancel(ctx)
	if err := r.startP2P(); err != nil {
		r.cancel()
		return err
	}
	r.presence = newPresence(r)
	r.rooms = newRoomTopics(r)
	r.mailbox = newMailboxSet(r)
	r.go_(r.presence.run)
	r.go_(r.rooms.run)
	r.go_(r.dialer.run)
	if r.cfg.Inet != nil {
		face, err := newInetFace(r, *r.cfg.Inet)
		if err != nil {
			r.cancel()
			return err
		}
		r.inet = face
		r.go_(func() { face.run(r.ctx) })
		r.log.Info("client face listening", "addr", face.Addr())
	}
	if r.cfg.MetricsInterval > 0 {
		r.go_(r.runMetrics)
	}
	r.go_(r.runExpiry)
	now := unixNow()
	for _, d := range r.cfg.Devices {
		a, err := envelope.EncodeAddress(d)
		if err != nil {
			return fmt.Errorf("qtcd: device %q: %w", d, err)
		}
		if err := r.Heard(a, ViaLocal, now); err != nil {
			return err
		}
	}
	r.log.Info("station started", "id", r.host.ID(), "addrs", r.host.Addrs(), "caps", r.cfg.Caps)
	return nil
}

// Stop shuts everything down and waits for background work to finish.
func (r *Station) Stop() error {
	if r.cancel != nil {
		r.cancel()
	}
	// Close store streams before waiting: event loops end only when their
	// stream does.
	if r.mailbox != nil {
		r.mailbox.closeAll()
	}
	r.wg.Wait()
	var errs []error
	if r.dht != nil {
		errs = append(errs, r.dht.Close())
	}
	if r.host != nil {
		errs = append(errs, r.host.Close())
	}
	return errors.Join(errs...)
}

func (r *Station) go_(f func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		f()
	}()
}

// ID is the node's libp2p peer ID. Valid after Start; see PeerID otherwise.
func (r *Station) ID() peer.ID { return r.host.ID() }

// PeerID derives the node's peer ID from its key without starting.
func (r *Station) PeerID() (peer.ID, error) {
	k, err := libp2pKey(r.key)
	if err != nil {
		return "", err
	}
	return peer.IDFromPrivateKey(k)
}

// AddrInfo is the node's ID and listen addresses, for other nodes' Bootstrap.
func (r *Station) AddrInfo() peer.AddrInfo {
	return peer.AddrInfo{ID: r.host.ID(), Addrs: r.host.Addrs()}
}

// Callsign is the node callsign.
func (r *Station) Callsign() envelope.Address { return r.callsign }

// Subscriptions exposes room subscription state.
func (r *Station) Subscriptions() *Subscriptions { return r.subs }

// Presence exposes the station table built from presence messages.
func (r *Station) Presence() *presence { return r.presence }

func unixNow() uint32 { return uint32(time.Now().Unix()) }

// deliverTo hands an envelope to every delivery sink for a device.
func (r *Station) deliverTo(device envelope.Address, e *envelope.Envelope) {
	r.cfg.Deliver(device, e)
	if r.inet != nil {
		r.inet.deliver(device, e)
	}
}

func (r *Station) runExpiry() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			now := unixNow()
			if r.mem != nil {
				if n := r.mem.Expire(now); n > 0 {
					r.log.Debug("expired stored envelopes", "n", n)
				}
			}
			r.subs.Expire(now)
			r.presence.expire(now)
		}
	}
}
