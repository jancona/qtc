package qtcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
)

// recordStore reads and writes mailbox records (node protocol §4, §7.3,
// §8) through the DHT and the announcement topic, and keeps the highest
// version accepted per callsign so a stale record cannot roll one back.
type recordStore struct {
	r     *Station
	topic *pubsub.Topic // set before any goroutine starts; nil if the join failed

	mu      sync.Mutex
	cache   map[envelope.Address]*MailboxRecord
	fetched map[envelope.Address]time.Time // last DHT read per callsign
}

// newRecordStore joins the announcement topic here, not in run, so that a
// record written while run is starting never races the join.
func newRecordStore(r *Station) *recordStore {
	rs := &recordStore{r: r, cache: map[envelope.Address]*MailboxRecord{}, fetched: map[envelope.Address]time.Time{}}
	t, err := r.ps.Join(MailboxRecordsTopic)
	if err != nil {
		r.log.Error("join mailbox records topic", "err", err)
	}
	rs.topic = t
	return rs
}

// run applies the records it hears on the announcement topic.
func (rs *recordStore) run() {
	if rs.topic == nil {
		return
	}
	sub, err := rs.topic.Subscribe()
	if err != nil {
		rs.r.log.Error("subscribe mailbox records topic", "err", err)
		return
	}
	defer sub.Cancel()
	for {
		m, err := sub.Next(rs.r.ctx)
		if err != nil {
			return
		}
		if m.GetFrom() == rs.r.host.ID() {
			continue
		}
		var rec MailboxRecord
		if err := json.Unmarshal(m.Data, &rec); err != nil {
			rs.r.log.Debug("bad mailbox record announcement", "from", m.GetFrom(), "err", err)
			continue
		}
		if err := rec.Verify(); err != nil {
			rs.r.log.Warn("mailbox record announcement does not verify", "from", m.GetFrom(), "err", err)
			continue
		}
		if rs.accept(&rec) {
			rs.r.log.Info("mailbox record announced", "callsign", rec.Callsign, "version", rec.Version, "home_station", rec.HomeStation, "members", rec.Members)
		}
	}
}

// accept stores a verified record if its version is newer than the cached
// one, reporting whether it was.
func (rs *recordStore) accept(rec *MailboxRecord) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	base := rec.Callsign.Base()
	if cur := rs.cache[base]; cur != nil && rec.Version <= cur.Version {
		return false
	}
	rs.cache[base] = rec
	return true
}

// cached returns the cached record, if any.
func (rs *recordStore) cached(base envelope.Address) *MailboxRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.cache[base]
}

// get returns the record for a base callsign, reading the DHT when the
// cache is empty or older than the refresh interval. It returns nil, nil
// when there is no record anywhere.
func (rs *recordStore) get(ctx context.Context, base envelope.Address) (*MailboxRecord, error) {
	base = base.Base()
	rs.mu.Lock()
	cur := rs.cache[base]
	fresh := time.Since(rs.fetched[base]) < rs.r.cfg.RecordRefresh
	rs.mu.Unlock()
	if cur != nil && fresh {
		return cur, nil
	}
	if rs.r.dht == nil {
		return cur, nil
	}
	value, err := rs.r.dht.GetValue(ctx, RecordKey(base))
	rs.mu.Lock()
	rs.fetched[base] = time.Now()
	rs.mu.Unlock()
	switch {
	case errors.Is(err, routing.ErrNotFound):
		return cur, nil
	case err != nil:
		if cur != nil {
			rs.r.log.Debug("mailbox record read failed; using cached", "callsign", base, "err", err)
			return cur, nil
		}
		return nil, fmt.Errorf("qtcd: read mailbox record for %s: %w", base, err)
	}
	rec, err := parseRecord(RecordKey(base), value)
	if err != nil {
		return cur, err
	}
	rs.accept(rec)
	return rs.cached(base), nil
}

// write signs rec with the node key, stores it in the DHT, announces it,
// and caches it. The caller sets Version to one more than the record it
// replaces. Every write is logged with the old and new versions.
func (rs *recordStore) write(ctx context.Context, rec *MailboxRecord, reason string) error {
	rec.Callsign = rec.Callsign.Base()
	rec.Updated = unixNow()
	if err := rec.Sign(rs.r.key); err != nil {
		return err
	}
	old := rs.cached(rec.Callsign)
	var oldVersion uint64
	var oldHome peer.ID
	if old != nil {
		oldVersion, oldHome = old.Version, old.HomeStation
	}
	if !rs.accept(rec) {
		return fmt.Errorf("%w: %d after %d", ErrRecordStale, rec.Version, oldVersion)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	rs.r.log.Info("mailbox record written", "callsign", rec.Callsign, "reason", reason,
		"old_version", oldVersion, "new_version", rec.Version, "old_home_station", oldHome,
		"home_station", rec.HomeStation, "members", rec.Members, "provisional", rec.Provisional())
	if rs.r.dht != nil {
		if err := rs.r.dht.PutValue(ctx, RecordKey(rec.Callsign), b); err != nil {
			rs.r.log.Warn("mailbox record DHT put failed; announced only", "callsign", rec.Callsign, "err", err)
		}
	}
	if rs.topic != nil {
		if err := rs.topic.Publish(ctx, b); err != nil {
			rs.r.log.Warn("mailbox record announce failed", "callsign", rec.Callsign, "err", err)
		}
	}
	return nil
}

// candidates returns mailbox-capable public stations for a record, most
// preferred first: connected stations from presence, then other presence
// stations, then configured seeds. This node counts if it is one. exclude
// lists members already in the record.
func (rs *recordStore) candidates(exclude []peer.ID) []peer.ID {
	seen := map[peer.ID]bool{}
	for _, id := range exclude {
		seen[id] = true
	}
	var connected, others []peer.ID
	add := func(id peer.ID) {
		if seen[id] {
			return
		}
		seen[id] = true
		if id == rs.r.host.ID() || rs.r.host.Network().Connectedness(id) == network.Connected {
			connected = append(connected, id)
		} else {
			others = append(others, id)
		}
	}
	if rs.r.cfg.Caps.Has(CapMailbox | CapPublic) {
		add(rs.r.host.ID())
	}
	for _, id := range rs.r.presence.Nodes() {
		if n, ok := rs.r.presence.Node(id); ok && n.Caps.Has(CapMailbox|CapPublic) {
			add(id)
		}
	}
	for _, id := range rs.r.mailbox.seeds {
		add(id)
	}
	return append(connected, others...)
}

// resolve returns the record to use for a base callsign, creating, taking
// over, or handing off as the node protocol requires. homed says whether
// this node currently hears the callsign.
func (rs *recordStore) resolve(ctx context.Context, base envelope.Address, homed bool) (*MailboxRecord, error) {
	base = base.Base()
	rec, err := rs.get(ctx, base)
	if err != nil {
		return nil, err
	}
	self := rs.r.host.ID()
	k := rs.r.cfg.K
	switch {
	case rec == nil:
		members := rs.candidates(nil)
		if len(members) > int(k) {
			members = members[:k]
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("qtcd: no mailbox-capable station known for %s", base)
		}
		rec = &MailboxRecord{Callsign: base, Version: 1, HomeStation: self, Members: members, K: k}
		reason := "created"
		if !homed {
			rec.Policy |= PolicyProvisional
			reason = "created on a sender's behalf"
		}
		if err := rs.write(ctx, rec, reason); err != nil {
			return nil, err
		}
		return rec, nil
	case homed && rec.HomeStation != self && rec.Provisional():
		// §7.3: the first station to hear the callsign takes over a
		// sender-created record, regardless of the takeover period.
		next := *rec
		next.Version, next.HomeStation, next.Policy = rec.Version+1, self, rec.Policy&^PolicyProvisional
		if err := rs.write(ctx, &next, "provisional handoff"); err != nil {
			return nil, err
		}
		return &next, nil
	case homed && rec.HomeStation != self && rs.silentFor(rec.HomeStation) > rs.r.cfg.TakeoverPeriod:
		next := *rec
		next.Version, next.HomeStation = rec.Version+1, self
		rs.r.log.Warn("taking over mailbox record", "callsign", base, "old_home_station", rec.HomeStation, "silent_for", rs.silentFor(rec.HomeStation).Round(time.Minute))
		if err := rs.write(ctx, &next, "takeover"); err != nil {
			return nil, err
		}
		return &next, nil
	}
	return rec, nil
}

// silentFor is how long since presence last saw a node. A node this
// station has never seen counts as silent only since this station started
// listening: a fresh station knows nothing about earlier silence and must
// not take over or repair on that basis.
func (rs *recordStore) silentFor(id peer.ID) time.Duration {
	if id == rs.r.host.ID() {
		return 0
	}
	n, ok := rs.r.presence.Node(id)
	if !ok || n.LastSeen == 0 {
		return time.Since(rs.r.started)
	}
	return time.Since(time.Unix(int64(n.LastSeen), 0))
}

// repair replaces failed members of a record this node is home station
// for (node protocol §8.2). failed lists members that have been
// unreachable for enough sweeps; union is the callsign's retained history
// to copy to the replacements. It returns the new record, or nil if no
// change was made.
func (rs *recordStore) repair(ctx context.Context, rec *MailboxRecord, failed []peer.ID, union map[envelope.ID]*envelope.Envelope) (*MailboxRecord, error) {
	var drop []peer.ID
	for _, id := range failed {
		if rec.hasMember(id) && rs.silentFor(id) > rs.r.cfg.MemberFailSilence {
			drop = append(drop, id)
		}
	}
	if len(drop) == 0 && len(rec.Members) >= int(rec.K) {
		return nil, nil
	}
	keep := make([]peer.ID, 0, len(rec.Members))
	for _, m := range rec.Members {
		dropped := false
		for _, d := range drop {
			if m == d {
				dropped = true
			}
		}
		if !dropped {
			keep = append(keep, m)
		}
	}
	cands := rs.candidates(append(append([]peer.ID(nil), rec.Members...), drop...))
	for len(keep) < int(rec.K) && len(cands) > 0 {
		id := cands[0]
		cands = cands[1:]
		for _, e := range union {
			rs.r.mailbox.putRetry(id, rec.Callsign, e, nil, nil)
		}
		keep = append(keep, id)
	}
	if equalMembers(keep, rec.Members) {
		return nil, nil
	}
	next := *rec
	next.Version, next.Members = rec.Version+1, keep
	rs.r.log.Warn("repairing mailbox record", "callsign", rec.Callsign, "dropped", drop, "members", keep, "k", rec.K)
	if err := rs.write(ctx, &next, "repair"); err != nil {
		return nil, err
	}
	return &next, nil
}
