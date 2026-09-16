package roost

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jancona/pigeon/envelope"
	"github.com/jancona/pigeon/store"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// inboxSet manages this node's store connections to inbox members and the
// PUT-with-retry queue (node protocol §6). During the spike every callsign
// has the same, statically configured member set.
type inboxSet struct {
	r       *Roost
	members []peer.ID

	mu    sync.Mutex
	conns map[peer.ID]*memberConn
}

// memberConn is one store stream to one inbox node, reopened on failure.
type memberConn struct {
	set    *inboxSet
	id     peer.ID
	mu     sync.Mutex
	stream network.Stream
	cl     *store.Client
	// watched is re-sent after a reconnect.
	watched map[envelope.Address]struct{}
}

func newInboxSet(r *Roost) *inboxSet {
	s := &inboxSet{r: r, conns: map[peer.ID]*memberConn{}}
	for _, m := range r.cfg.InboxMembers {
		id, err := peer.Decode(m)
		if err != nil {
			r.log.Error("bad inbox member peer ID", "member", m, "err", err)
			continue
		}
		s.members = append(s.members, id)
	}
	return s
}

// membersFor returns the inbox members for a base callsign. Static during
// the spike; will read the inbox record later.
func (s *inboxSet) membersFor(envelope.Address) []peer.ID { return s.members }

func (s *inboxSet) conn(id peer.ID) *memberConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.conns[id]
	if c == nil {
		c = &memberConn{set: s, id: id, watched: map[envelope.Address]struct{}{}}
		s.conns[id] = c
	}
	return c
}

func (s *inboxSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.close()
	}
}

// client returns a live store client, opening the stream if needed. On a
// fresh stream it re-issues WATCH for everything this connection watches
// and starts the event loop.
func (c *memberConn) client(ctx context.Context) (*store.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cl != nil {
		select {
		case <-c.cl.Done():
			c.stream.Close()
			c.cl, c.stream = nil, nil
		default:
			return c.cl, nil
		}
	}
	// A short dial timeout keeps a dead member from stalling a sweep.
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, err := c.set.r.openStore(dctx, c.id)
	if err != nil {
		return nil, err
	}
	cl := store.NewClient(s, c.set.r.log)
	c.stream, c.cl = s, cl
	if len(c.watched) > 0 {
		calls := make([]envelope.Address, 0, len(c.watched))
		for a := range c.watched {
			calls = append(calls, a)
		}
		if err := cl.Watch(calls...); err != nil {
			s.Close()
			c.cl, c.stream = nil, nil
			return nil, err
		}
	}
	c.set.r.go_(func() { c.set.r.eventLoop(c.id, cl) })
	return cl, nil
}

func (c *memberConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream != nil {
		c.stream.Close()
	}
}

// watch adds callsigns to this connection's WATCH set and sends it.
func (c *memberConn) watch(ctx context.Context, calls ...envelope.Address) error {
	c.mu.Lock()
	for _, a := range calls {
		c.watched[a.Base()] = struct{}{}
	}
	c.mu.Unlock()
	cl, err := c.client(ctx)
	if err != nil {
		return err
	}
	return cl.Watch(calls...)
}

// eventLoop delivers EVENTs from one member until the stream ends, then
// reconnects so the WATCH is restored, and sweeps to pick up anything
// stored while the stream was down.
func (r *Roost) eventLoop(id peer.ID, cl *store.Client) {
	for ev := range cl.Events() {
		r.onStored(ev.Callsign, ev.Env, id)
	}
	r.log.Info("store stream closed", "member", id, "err", cl.Err())
	r.inbox.rewatch(id)
}

// rewatch reopens the stream to a member with backoff until it succeeds
// (client re-sends the WATCH set), then sweeps every homed callsign.
func (s *inboxSet) rewatch(id peer.ID) {
	c := s.conn(id)
	c.mu.Lock()
	n := len(c.watched)
	c.mu.Unlock()
	if n == 0 {
		return
	}
	delay := 5 * time.Second
	for {
		select {
		case <-s.r.ctx.Done():
			return
		case <-time.After(delay):
		}
		ctx, cancel := context.WithTimeout(s.r.ctx, 30*time.Second)
		_, err := c.client(ctx)
		cancel()
		if err == nil {
			s.r.log.Info("store stream reopened", "member", id)
			break
		}
		s.r.log.Debug("store reconnect failed", "member", id, "err", err, "retry_in", delay)
		delay = min(delay*2, time.Minute)
	}
	s.r.mu.Lock()
	homed := make([]*homed, 0, len(s.r.homed))
	for _, h := range s.r.homed {
		homed = append(homed, h)
	}
	s.r.mu.Unlock()
	for _, h := range homed {
		s.r.sweep(h)
	}
}

// putAll stores e under callsign on every inbox member, retrying each until
// it accepts or the envelope expires. onFirst runs once, when the first
// member accepts.
func (s *inboxSet) putAll(callsign envelope.Address, e *envelope.Envelope, onFirst func()) {
	base := callsign.Base()
	members := s.membersFor(base)
	if len(members) == 0 {
		s.r.log.Warn("no inbox members configured; envelope not stored", "callsign", base, "envelope", e)
		return
	}
	var once sync.Once
	for _, id := range members {
		id := id
		s.r.go_(func() { s.putRetry(id, base, e, &once, onFirst) })
	}
}

func (s *inboxSet) putRetry(id peer.ID, base envelope.Address, e *envelope.Envelope, once *sync.Once, onFirst func()) {
	deadline := s.expiry(e)
	delay := 2 * time.Second
	for {
		err := s.putOnce(id, base, e)
		if err == nil {
			if onFirst != nil {
				once.Do(onFirst)
			}
			return
		}
		var pe *store.PutError
		if errors.As(err, &pe) {
			// The member answered; retrying a refusal is pointless.
			s.r.log.Warn("inbox refused envelope", "member", id, "callsign", base, "id", e.StoreID(), "code", pe.Code, "reason", pe.Reason)
			return
		}
		if unixNow() >= deadline {
			s.r.log.Warn("giving up storing envelope", "member", id, "callsign", base, "id", e.StoreID(), "err", err)
			return
		}
		s.r.log.Debug("put failed; will retry", "member", id, "err", err, "in", delay)
		select {
		case <-s.r.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

func (s *inboxSet) putOnce(id peer.ID, base envelope.Address, e *envelope.Envelope) error {
	ctx, cancel := context.WithTimeout(s.r.ctx, 30*time.Second)
	defer cancel()
	cl, err := s.conn(id).client(ctx)
	if err != nil {
		return err
	}
	return cl.Put(ctx, base, e)
}

// expiry is how long to keep retrying a PUT: the envelope's own expiry when
// known, otherwise the default TTL from now.
func (s *inboxSet) expiry(e *envelope.Envelope) uint32 {
	if m, ok := e.Msg(); ok {
		if exp, ok := m.Expiry(); ok {
			return exp
		}
	}
	return unixNow() + uint32(s.r.cfg.DefaultTTL)*60
}

// query runs QueryAll against one member.
func (s *inboxSet) query(ctx context.Context, id peer.ID, base envelope.Address, since uint32) ([]*envelope.Envelope, error) {
	cl, err := s.conn(id).client(ctx)
	if err != nil {
		return nil, err
	}
	return cl.QueryAll(ctx, base, since, []envelope.PacketType{envelope.TypeMSG, envelope.TypeRCPT, envelope.TypeROOM})
}
