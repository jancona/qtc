package roost

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jancona/pigeon/envelope"
)

// deliveries collects what a roost delivers to local devices.
type deliveries struct {
	mu   sync.Mutex
	list []delivery
}

type delivery struct {
	device envelope.Address
	env    *envelope.Envelope
}

func (d *deliveries) add(device envelope.Address, e *envelope.Envelope) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list = append(d.list, delivery{device, e})
}

func (d *deliveries) find(pred func(delivery) bool) (delivery, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, x := range d.list {
		if pred(x) {
			return x, true
		}
	}
	return delivery{}, false
}

func (d *deliveries) count(pred func(delivery) bool) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, x := range d.list {
		if pred(x) {
			n++
		}
	}
	return n
}

func testLog(t *testing.T, name string) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("roost", name)
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startRoost(t *testing.T, cfg Config, d *deliveries) *Roost {
	t.Helper()
	cfg.ListenAddrs = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.Log = testLog(t, cfg.Callsign)
	cfg.Deliver = d.add
	cfg.SweepInterval = 5 * time.Second
	cfg.PresenceInterval = 3 * time.Second
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Stop(); err != nil {
			t.Logf("stop %s: %v", cfg.Callsign, err)
		}
	})
	return r
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustMsg(t *testing.T, src, dst envelope.Address, ts uint32, ttl, nonce uint16, flags byte, body string) *envelope.Envelope {
	t.Helper()
	e, err := envelope.BuildMsg(src, dst, ts, ttl, nonce, flags, body)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestThreeNodeSpike runs the spike topology in one process: a public inbox
// node and two roosts, each homing one callsign. It checks the unicast
// path with receipts, presence, and a room message.
func TestThreeNodeSpike(t *testing.T) {
	if testing.Short() {
		t.Skip("network-heavy")
	}
	var dP, dA, dB deliveries
	pub := startRoost(t, Config{Callsign: "K1XYZ  R", Caps: CapPublic | CapRelay | CapInbox, Software: "test"}, &dP)
	pubAddr := pub.AddrInfo()
	var bootstrap []string
	for _, a := range pubAddr.Addrs {
		bootstrap = append(bootstrap, a.String()+"/p2p/"+pubAddr.ID.String())
	}
	common := Config{Bootstrap: bootstrap, InboxMembers: []string{pubAddr.ID.String()}, Software: "test"}

	cfgA := common
	cfgA.Callsign, cfgA.Devices = "N1ADJ  R", []string{"N1ADJ  H"}
	a := startRoost(t, cfgA, &dA)
	cfgB := common
	cfgB.Callsign, cfgB.Devices = "W1AW  R", []string{"W1AW"}
	b := startRoost(t, cfgB, &dB)
	// A and B are not connected directly; they must find each other from
	// presence and connect through the public node's relay (or hole punch).

	n1adjH := mustAddr(t, "N1ADJ  H")
	w1aw := mustAddr(t, "W1AW")

	// Both roosts home their callsign and watch it on the inbox node.
	eventually(t, "watches on the inbox node", 20*time.Second, func() bool {
		return pub.server.Watchers(n1adjH) == 1 && pub.server.Watchers(w1aw) == 1
	})

	// Unicast with receipt: N1ADJ's HT sends to W1AW.
	now := unixNow()
	msg := mustMsg(t, n1adjH, w1aw, now, 60, 0x1234, envelope.FlagRcptReq, "hello from the spike")
	if err := a.Send(msg); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivery to W1AW", 20*time.Second, func() bool {
		_, ok := dB.find(func(d delivery) bool { return d.device == w1aw && d.env.ID() == msg.ID() })
		return ok
	})
	isRcpt := func(st envelope.Status) func(delivery) bool {
		return func(d delivery) bool {
			rc, ok := d.env.Rcpt()
			return ok && d.device == n1adjH && rc.MessageID() == msg.ID() && rc.Status() == st
		}
	}
	eventually(t, "QUEUED receipt at N1ADJ", 20*time.Second, func() bool {
		_, ok := dA.find(isRcpt(envelope.StatusQueued))
		return ok
	})
	eventually(t, "TRANSMITTED receipt at N1ADJ", 20*time.Second, func() bool {
		_, ok := dA.find(isRcpt(envelope.StatusTransmitted))
		return ok
	})
	if rc, _ := dA.find(isRcpt(envelope.StatusTransmitted)); rc.env.Source() != b.Callsign() {
		t.Errorf("TRANSMITTED from %s, want %s", rc.env.Source(), b.Callsign())
	}
	// Inbox node holds the message under both callsigns plus the receipts.
	if n := pub.mem.Len(); n < 4 {
		t.Errorf("inbox holds %d envelopes, want at least 4", n)
	}

	// Never twice: a sweep on B must not redeliver.
	time.Sleep(6 * time.Second)
	if n := dB.count(func(d delivery) bool { return d.env.ID() == msg.ID() }); n != 1 {
		t.Errorf("W1AW received the message %d times", n)
	}
	if n := dA.count(isRcpt(envelope.StatusQueued)); n != 1 {
		t.Errorf("QUEUED delivered %d times", n)
	}

	// Presence: A learns that B roosts W1AW, and both see the public node's card.
	eventually(t, "presence", 20*time.Second, func() bool {
		roosts := a.Presence().Roosts(w1aw)
		if len(roosts) != 1 || roosts[0] != b.ID() {
			return false
		}
		n, ok := a.Presence().Node(pub.ID())
		return ok && n.Caps.Has(CapInbox) && n.Callsign == pub.Callsign()
	})

	// Rooms: N1ADJ joins MAINE explicitly; W1AW sends to it implicitly.
	maine := mustRoom(t, "MAINE")
	reply, err := a.HandleRoom(n1adjH, mustRoomPkt(t, envelope.OpJoin, 0, maine))
	if err != nil {
		t.Fatal(err)
	}
	if rr, _ := reply.Room(); rr.Op() != envelope.OpOK {
		t.Fatalf("JOIN reply %s", reply)
	}
	eventually(t, "A joined the MAINE topic", 10*time.Second, func() bool {
		a.rooms.mu.Lock()
		defer a.rooms.mu.Unlock()
		return a.rooms.joined[maine] != nil
	})
	time.Sleep(2 * time.Second) // let the subscription announcement reach B
	roomMsg := mustMsg(t, w1aw, maine, unixNow(), 60, 7, 0, "net tonight")
	if err := b.Send(roomMsg); err != nil {
		t.Fatal(err)
	}
	eventually(t, "room message at N1ADJ", 20*time.Second, func() bool {
		_, ok := dA.find(func(d delivery) bool { return d.device == n1adjH && d.env.ID() == roomMsg.ID() })
		return ok
	})
	// The sender is a subscriber too, so B delivered it to W1AW as well.
	if _, ok := dB.find(func(d delivery) bool { return d.device == w1aw && d.env.ID() == roomMsg.ID() }); !ok {
		t.Error("room message not delivered to its sender's device")
	}
	// Room messages never produce receipts.
	if n := dB.count(func(d delivery) bool { rc, ok := d.env.Rcpt(); return ok && rc.MessageID() == roomMsg.ID() }); n != 0 {
		t.Errorf("room message produced %d receipts", n)
	}
	// W1AW's implicit JOIN was stored and reached B's subscription state.
	if rooms, _ := b.Subscriptions().Rooms(w1aw); !containsAddr(rooms, maine) {
		t.Errorf("W1AW rooms = %v, want MAINE", rooms)
	}
}

func containsAddr(list []envelope.Address, a envelope.Address) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

func TestKeyFile(t *testing.T) {
	path := t.TempDir() + "/node.key"
	k1, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Equal(k2) {
		t.Error("key changed between loads")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", fi.Mode().Perm())
	}
	if _, err := libp2pKey(k1); err != nil {
		t.Error(err)
	}
	if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateKey(path); err == nil {
		t.Error("accepted a junk key file")
	}
}

func TestParseAddrInfos(t *testing.T) {
	ais, err := parseAddrInfos([]string{
		"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWEjRzKjmn3sQ8Gn6Gp6mKb8RoJRVUnzUD8bVHpYwi3Wqc",
		"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWEjRzKjmn3sQ8Gn6Gp6mKb8RoJRVUnzUD8bVHpYwi3Wqc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ais) != 1 || len(ais[0].Addrs) != 2 {
		t.Errorf("got %v", ais)
	}
	if _, err := parseAddrInfos([]string{"/ip4/127.0.0.1/tcp/4001"}); err == nil {
		t.Error("accepted an address without a peer ID")
	}
}
