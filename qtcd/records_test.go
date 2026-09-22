package qtcd

import (
	"context"
	"testing"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/libp2p/go-libp2p/core/peer"
)

// startPublic starts a public mailbox station with DHT in server mode.
func startPublic(t *testing.T, callsign string, d *deliveries, devices ...string) *Station {
	t.Helper()
	return startStation(t, Config{Callsign: callsign, Caps: CapPublic | CapRelay | CapMailbox, Software: "test", EnableDHT: true, Devices: devices}, d)
}

func bootstrapOf(s *Station) []string {
	ai := s.AddrInfo()
	var out []string
	for _, a := range ai.Addrs {
		out = append(out, a.String()+"/p2p/"+ai.ID.String())
	}
	return out
}

func recordOf(t *testing.T, s *Station, base envelope.Address) *MailboxRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec, err := s.records.get(ctx, base)
	if err != nil {
		t.Fatalf("record for %s at %s: %v", base, s.Callsign(), err)
	}
	return rec
}

// TestMailboxRecords covers creation by the first station to home a
// callsign, lookup by a sender through the DHT, a sender-created
// provisional record handed off to the station that hears the callsign,
// and repair when a member fails.
func TestMailboxRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("network-heavy")
	}
	var dP1, dP2, dP3, dA, dB deliveries
	p1 := startPublic(t, "K1XYZ  R", &dP1)
	boot := bootstrapOf(p1)
	p2 := startStation(t, Config{Callsign: "K1XYZ  S", Caps: CapPublic | CapRelay | CapMailbox, Software: "test", EnableDHT: true, Bootstrap: boot}, &dP2)
	p3 := startStation(t, Config{Callsign: "K1XYZ  T", Caps: CapPublic | CapRelay | CapMailbox, Software: "test", EnableDHT: true, Bootstrap: boot}, &dP3)
	// Short periods so repair happens within the test.
	fast := func(c Config) Config {
		c.SweepInterval = 3 * time.Second
		c.MemberFailSweeps = 2
		c.MemberFailSilence = 2 * time.Second
		c.RecordRefresh = 2 * time.Second
		return c
	}
	a := startStation(t, fast(Config{Callsign: "N1ADJ  R", Bootstrap: boot, Software: "test", EnableDHT: true, K: 2, Devices: []string{"N1ADJ  H"}}), &dA)
	b := startStation(t, fast(Config{Callsign: "W1AW  R", Bootstrap: boot, Software: "test", EnableDHT: true, K: 2, Devices: []string{"W1AW"}}), &dB)
	for _, s := range []*Station{p2, p3, a, b} {
		for _, o := range []*Station{p1, p2, p3} {
			if s != o {
				_ = s.Connect(context.Background(), o.AddrInfo())
			}
		}
	}
	n1adj, w1aw := mustAddr(t, "N1ADJ"), mustAddr(t, "W1AW")
	publics := map[peer.ID]bool{p1.ID(): true, p2.ID(): true, p3.ID(): true}

	// A created N1ADJ's record with itself as home station and two
	// mailbox-capable public stations as members.
	var recA *MailboxRecord
	eventually(t, "A's record for N1ADJ", 45*time.Second, func() bool {
		recA = a.records.cached(n1adj)
		return recA != nil && recA.HomeStation == a.ID() && len(recA.Members) == 2
	})
	for _, m := range recA.Members {
		if !publics[m] {
			t.Errorf("member %s is not a public mailbox station", m)
		}
	}
	if recA.Provisional() || recA.Version != 1 || recA.K != 2 {
		t.Errorf("record = %+v", recA)
	}
	if err := recA.Verify(); err != nil {
		t.Error(err)
	}

	// B reads it through the DHT and delivers a message via those members.
	eventually(t, "B reads N1ADJ's record from the DHT", 45*time.Second, func() bool {
		rec := recordOf(t, b, n1adj)
		return rec != nil && rec.Version == recA.Version && rec.HomeStation == a.ID()
	})
	msg := mustMsg(t, w1aw, mustAddr(t, "N1ADJ  H"), unixNow(), 60, 0x31, 0, "via the record")
	if err := b.Send(msg); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delivery through record members", 30*time.Second, func() bool {
		_, ok := dA.find(func(d delivery) bool { return d.env.ID() == msg.ID() })
		return ok
	})

	// Provisional: B sends to AB1CD, whom nobody has heard. B creates a
	// provisional record naming itself; when P1 hears AB1CD it takes over.
	ab1cd := mustAddr(t, "AB1CD")
	if err := b.Send(mustMsg(t, w1aw, ab1cd, unixNow(), 60, 0x32, 0, "first contact")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "provisional record by B", 30*time.Second, func() bool {
		rec := b.records.cached(ab1cd)
		return rec != nil && rec.Provisional() && rec.HomeStation == b.ID()
	})
	if err := p1.Heard(ab1cd, ViaLocal, unixNow()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "handoff to P1", 45*time.Second, func() bool {
		rec := p1.records.cached(ab1cd)
		return rec != nil && !rec.Provisional() && rec.HomeStation == p1.ID() && rec.Version == 2
	})
	eventually(t, "B learns of the handoff", 45*time.Second, func() bool {
		rec := b.records.cached(ab1cd)
		return rec != nil && rec.HomeStation == p1.ID()
	})
	// The stored first contact reaches AB1CD's device at P1 by sweep.
	eventually(t, "first contact delivered after handoff", 45*time.Second, func() bool {
		_, ok := dP1.find(func(d delivery) bool { m, ok := d.env.Msg(); return ok && m.Body() == "first contact" })
		return ok
	})

	// Repair: stop one of N1ADJ's members. After two failed sweeps and the
	// silence period, A (home station) replaces it with the remaining
	// public station and copies the history there.
	var victim, survivor *Station
	for _, s := range []*Station{p1, p2, p3} {
		if recA.hasMember(s.ID()) && victim == nil && s != p1 {
			victim = s
		}
	}
	if victim == nil {
		for _, s := range []*Station{p2, p3} {
			if recA.hasMember(s.ID()) {
				victim = s
			}
		}
	}
	for _, s := range []*Station{p1, p2, p3} {
		if !recA.hasMember(s.ID()) {
			survivor = s
		}
	}
	if victim == nil || survivor == nil {
		t.Fatalf("members %v leave no victim/survivor among the public stations", recA.Members)
	}
	if err := victim.Stop(); err != nil {
		t.Logf("stop victim: %v", err)
	}
	eventually(t, "repair writes a new record", 90*time.Second, func() bool {
		rec := a.records.cached(n1adj)
		return rec != nil && rec.Version == 2 && !rec.hasMember(victim.ID()) && rec.hasMember(survivor.ID()) && len(rec.Members) == 2
	})
	eventually(t, "history copied to the recruit", 30*time.Second, func() bool {
		return survivor.mem.Len() > 0
	})
}
