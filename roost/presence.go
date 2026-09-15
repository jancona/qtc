package roost

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/jancona/pigeon/envelope"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Wire forms for node protocol §2 and §3.
type nodeCard struct {
	Callsign string `json:"callsign"`
	Caps     int    `json:"caps"`
	Software string `json:"software"`
}

type heardEntry struct {
	Device string `json:"device"`
	Via    int    `json:"via"`
	Last   uint32 `json:"last"`
}

type presenceMsg struct {
	Node  string       `json:"node"`
	Card  nodeCard     `json:"card"`
	Time  uint32       `json:"time"`
	Heard []heardEntry `json:"heard"`
}

// Expiry rules from node protocol §3.
const (
	presenceEntryExpiry = 30 * 24 * 3600 // seconds since last heard
	silentNodeExpiry    = 7 * 24 * 3600  // seconds since the node last published
	heardPublishMin     = 5 * 60         // per-device republish floor
)

// DeviceInfo is one heard entry in the roost table.
type DeviceInfo struct {
	Via  Via
	Last uint32
}

// NodeInfo is what presence has told us about another node.
type NodeInfo struct {
	Card     nodeCard
	Caps     Capabilities
	Callsign envelope.Address
	LastSeen uint32
	Devices  map[envelope.Address]DeviceInfo
}

// presence publishes this node's presence and keeps the roost table.
type presence struct {
	r     *Roost
	topic *pubsub.Topic

	mu    sync.Mutex
	local map[envelope.Address]*localDevice // devices this node hears
	dirty bool
	nodes map[peer.ID]*NodeInfo
}

type localDevice struct {
	via           Via
	last          uint32
	lastPublished uint32
	publishedVia  Via
}

func newPresence(r *Roost) *presence {
	return &presence{r: r, local: map[envelope.Address]*localDevice{}, nodes: map[peer.ID]*NodeInfo{}}
}

// heard records a local device. It returns true when this changes what the
// next presence message will carry.
func (p *presence) heard(device envelope.Address, via Via, now uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.local[device]
	if d == nil {
		d = &localDevice{}
		p.local[device] = d
		p.dirty = true
	}
	if via != d.publishedVia {
		p.dirty = true
	}
	d.via, d.last = via, now
}

// lastHeard returns when a local device was last heard; 0 if never.
func (p *presence) lastHeard(device envelope.Address) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.local[device]; d != nil {
		return d.last
	}
	return 0
}

// localDevices returns the local devices of a base callsign.
func (p *presence) localDevices(base envelope.Address) []envelope.Address {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []envelope.Address
	for a := range p.local {
		if a.Base() == base {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (p *presence) run() {
	t, err := p.r.ps.Join(PresenceTopic)
	if err != nil {
		p.r.log.Error("join presence topic", "err", err)
		return
	}
	p.topic = t
	sub, err := t.Subscribe()
	if err != nil {
		p.r.log.Error("subscribe presence topic", "err", err)
		return
	}
	defer sub.Cancel()
	p.r.go_(func() {
		for {
			m, err := sub.Next(p.r.ctx)
			if err != nil {
				return
			}
			if m.ReceivedFrom == p.r.host.ID() || m.GetFrom() == p.r.host.ID() {
				continue
			}
			p.receive(m)
		}
	})

	// Publish on change (checked every few seconds) and at the interval,
	// with or without local devices: the card must be discoverable.
	check := time.NewTicker(2 * time.Second)
	defer check.Stop()
	var lastPublish time.Time
	for {
		select {
		case <-p.r.ctx.Done():
			return
		case <-check.C:
		}
		p.mu.Lock()
		due := p.dirty || time.Since(lastPublish) >= p.r.cfg.PresenceInterval
		p.mu.Unlock()
		if !due {
			continue
		}
		if err := p.publish(unixNow()); err != nil {
			p.r.log.Warn("publish presence", "err", err)
			continue
		}
		lastPublish = time.Now()
	}
}

// publish sends a presence message. Every device is included when it is
// new or its via changed; otherwise at most once per five minutes.
func (p *presence) publish(now uint32) error {
	p.mu.Lock()
	msg := presenceMsg{
		Node: p.r.host.ID().String(),
		Card: nodeCard{Callsign: p.r.cfg.Callsign, Caps: int(p.r.cfg.Caps), Software: p.r.cfg.Software},
		Time: now,
	}
	for a, d := range p.local {
		if d.lastPublished != 0 && d.publishedVia == d.via && now-d.lastPublished < heardPublishMin {
			continue
		}
		msg.Heard = append(msg.Heard, heardEntry{Device: a.String(), Via: int(d.via), Last: d.last})
		d.lastPublished, d.publishedVia = now, d.via
	}
	p.dirty = false
	p.mu.Unlock()
	sort.Slice(msg.Heard, func(i, j int) bool { return msg.Heard[i].Device < msg.Heard[j].Device })
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return p.topic.Publish(p.r.ctx, b)
}

// receive applies another node's presence message. The node identity is the
// gossipsub signer; a mismatched "node" field is ignored (§3).
func (p *presence) receive(m *pubsub.Message) {
	var msg presenceMsg
	if err := json.Unmarshal(m.Data, &msg); err != nil {
		p.r.log.Debug("bad presence message", "from", m.GetFrom(), "err", err)
		return
	}
	from := m.GetFrom()
	if msg.Node != "" && msg.Node != from.String() {
		p.r.log.Warn("presence node field does not match signer", "signer", from, "claimed", msg.Node)
		return
	}
	callsign, err := envelope.EncodeAddress(msg.Card.Callsign)
	if err != nil {
		p.r.log.Debug("presence with bad node callsign", "from", from, "callsign", msg.Card.Callsign)
		return
	}
	now := unixNow()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.nodes[from]
	if n == nil {
		n = &NodeInfo{Devices: map[envelope.Address]DeviceInfo{}}
		p.nodes[from] = n
		p.r.log.Info("new node", "peer", from, "callsign", msg.Card.Callsign, "caps", msg.Card.Caps)
	}
	n.Card, n.Caps, n.Callsign, n.LastSeen = msg.Card, Capabilities(msg.Card.Caps), callsign, now
	for _, h := range msg.Heard {
		a, err := envelope.EncodeAddress(h.Device)
		if err != nil || !a.IsStandard() {
			continue
		}
		if cur, ok := n.Devices[a]; ok && cur.Last > h.Last {
			continue
		}
		n.Devices[a] = DeviceInfo{Via: Via(h.Via), Last: h.Last}
	}
}

// expire drops stale entries and silent nodes.
func (p *presence) expire(now uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, n := range p.nodes {
		if now-n.LastSeen > silentNodeExpiry {
			delete(p.nodes, id)
			continue
		}
		for a, d := range n.Devices {
			if now-d.Last > presenceEntryExpiry {
				delete(n.Devices, a)
			}
		}
	}
}

// Roosts returns the nodes with an unexpired entry for any device of base.
func (p *presence) Roosts(base envelope.Address) []peer.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []peer.ID
	for id, n := range p.nodes {
		for a := range n.Devices {
			if a.Base() == base {
				out = append(out, id)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Node returns what is known about a peer from presence.
func (p *presence) Node(id peer.ID) (NodeInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.nodes[id]
	if n == nil {
		return NodeInfo{}, false
	}
	cp := *n
	cp.Devices = make(map[envelope.Address]DeviceInfo, len(n.Devices))
	for a, d := range n.Devices {
		cp.Devices[a] = d
	}
	return cp, true
}

// Nodes returns the peer IDs of every node heard in presence.
func (p *presence) Nodes() []peer.ID {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]peer.ID, 0, len(p.nodes))
	for id := range p.nodes {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
