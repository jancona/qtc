package qtcd

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
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
	// generated if missing. Empty means DataDir/node.key, or an ephemeral key
	// when DataDir is empty too (tests).
	KeyFile string
	// DataDir holds the node's persistent state: the mailbox journal, the
	// delivered-once journal, and by default the key. Empty keeps all of it
	// in memory, so a restart loses stored mailboxes and may repeat recent
	// deliveries (tests).
	DataDir string
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

	// MailboxMembers are seed mailbox stations (peer IDs) used as record
	// members when presence has not yet shown any mailbox-capable station.
	MailboxMembers []string
	// K is the mailbox target size for records this node creates; 0 means 2.
	K uint8

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

	PresenceInterval  time.Duration // default 5 min
	SweepInterval     time.Duration // default 1 h
	ReachWindow       time.Duration // a device heard this recently is reachable; others' messages are held; default 1 h
	ReplayLimit       int           // most messages a device is sent on replay; default 10
	ActiveWindow      time.Duration // default 24 h
	MetricsInterval   time.Duration // 0 disables the metrics log
	RecordRefresh     time.Duration // how long a cached mailbox record is trusted before rereading the DHT; default 5 min
	TakeoverPeriod    time.Duration // home station silence before another station may take over; default 7 days
	MemberFailSilence time.Duration // member silence in presence before it counts as failed; default 24 h
	MemberFailSweeps  int           // unreachable sweeps before a member counts as failed; default 3
	DefaultTTL        uint16        // minutes; default 7 days

	Log *slog.Logger
}

func (c *Config) defaults() {
	if c.PresenceInterval == 0 {
		c.PresenceInterval = 5 * time.Minute
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = time.Hour
	}
	if c.ReachWindow == 0 {
		c.ReachWindow = time.Hour
	}
	if c.ReplayLimit <= 0 {
		c.ReplayLimit = 10
	}
	if c.ActiveWindow == 0 {
		c.ActiveWindow = 24 * time.Hour
	}
	if c.DefaultTTL == 0 {
		c.DefaultTTL = store.DefaultTTLMinutes
	}
	if c.K == 0 {
		c.K = 2
	}
	if c.RecordRefresh == 0 {
		c.RecordRefresh = 5 * time.Minute
	}
	if c.TakeoverPeriod == 0 {
		c.TakeoverPeriod = 7 * 24 * time.Hour
	}
	if c.MemberFailSilence == 0 {
		c.MemberFailSilence = 24 * time.Hour
	}
	if c.MemberFailSweeps == 0 {
		c.MemberFailSweeps = 3
	}
	if c.Software == "" {
		c.Software = "qtcd/0"
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.KeyFile == "" && c.DataDir != "" {
		c.KeyFile = filepath.Join(c.DataDir, "node.key")
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
	records  *recordStore
	inet     *inetFace
	mem      mailboxStore
	server   *store.Server

	delivered *deliveredTable
	held      map[envelope.Address]bool // devices with a message waiting; guarded by mu

	mu     sync.Mutex
	homed  map[envelope.Address]*homed // by base callsign
	queued map[envelope.ID]struct{}    // QUEUED already issued

	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time // when Start ran; bounds what this node can know about others' silence
	wg      sync.WaitGroup
}

// mailboxStore is this node's own mailbox: a store.MemStore, or a
// store.FileStore when the node has a DataDir.
type mailboxStore interface {
	store.Store
	Len() int
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
		delivered: newDeliveredTable(),
		held:      map[envelope.Address]bool{},
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
	r.started = time.Now()
	if r.cfg.DataDir != "" {
		// Opened here rather than in New so that New (and qtcd -print-id)
		// never touches state files.
		d, err := openDeliveredTable(filepath.Join(r.cfg.DataDir, "delivered.jsonl"), unixNow(), r.log)
		if err != nil {
			r.cancel()
			return err
		}
		r.delivered = d
	}
	if err := r.startP2P(); err != nil {
		r.cancel()
		r.closeState()
		return err
	}
	r.presence = newPresence(r)
	r.rooms = newRoomTopics(r)
	r.mailbox = newMailboxSet(r)
	r.records = newRecordStore(r)
	r.go_(r.records.run)
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
	errs = append(errs, r.closeState())
	return errors.Join(errs...)
}

// closeState closes the persistent state files, if any.
func (r *Station) closeState() error {
	var errs []error
	if fs, ok := r.mem.(*store.FileStore); ok {
		errs = append(errs, fs.Close())
	}
	errs = append(errs, r.delivered.close())
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

// deliverTo hands an envelope to a device and reports whether it went. A
// device heard through the M17_inet face needs its gateway or client link
// up; a device given by config or the admin interface always takes it.
func (r *Station) deliverTo(device envelope.Address, e *envelope.Envelope) bool {
	if via := r.presence.localVia(device); r.inet != nil && (via == ViaRF || via == ViaInternet) {
		if !r.inet.deliver(device, e) {
			return false
		}
	}
	r.cfg.Deliver(device, e)
	return true
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
			r.delivered.expire(now)
		}
	}
}
