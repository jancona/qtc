package roost

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// peerDialer keeps connections to the other roosts learned from presence so
// that gossipsub has a path for room topics. Addresses come from the
// peerstore, then the DHT, and finally a relayed address through each
// bootstrap relay, which a NATed roost can always reach; hole punching
// upgrades a relayed connection to a direct one when it can. Attempts back
// off per peer from 30 s to 5 min while the peer stays unconnected.
type peerDialer struct {
	r      *Roost
	relays []peer.AddrInfo

	mu    sync.Mutex
	state map[peer.ID]*dialState
}

type dialState struct {
	next    time.Time
	backoff time.Duration
	busy    bool
}

const (
	dialBackoffMin = 30 * time.Second
	dialBackoffMax = 5 * time.Minute
	dialPass       = 15 * time.Second
)

func newPeerDialer(r *Roost, relays []peer.AddrInfo) *peerDialer {
	return &peerDialer{r: r, relays: relays, state: map[peer.ID]*dialState{}}
}

// run periodically dials every presence node that is not connected.
func (d *peerDialer) run() {
	t := time.NewTicker(dialPass)
	defer t.Stop()
	for {
		select {
		case <-d.r.ctx.Done():
			return
		case <-t.C:
			for _, id := range d.r.presence.Nodes() {
				d.want(id)
			}
		}
	}
}

// want dials id now if it is not connected and its backoff has elapsed.
func (d *peerDialer) want(id peer.ID) {
	if id == d.r.host.ID() || d.r.host.Network().Connectedness(id) == network.Connected {
		d.mu.Lock()
		delete(d.state, id)
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	st := d.state[id]
	if st == nil {
		st = &dialState{backoff: dialBackoffMin}
		d.state[id] = st
	}
	if st.busy || time.Now().Before(st.next) {
		d.mu.Unlock()
		return
	}
	st.busy = true
	d.mu.Unlock()
	d.r.go_(func() {
		err := d.dial(id)
		d.mu.Lock()
		defer d.mu.Unlock()
		st.busy = false
		if err != nil {
			st.next = time.Now().Add(st.backoff)
			st.backoff = min(st.backoff*2, dialBackoffMax)
			d.r.log.Debug("dial roost failed", "peer", id, "err", err, "retry_in", time.Until(st.next).Round(time.Second))
		} else {
			delete(d.state, id)
		}
	})
}

func (d *peerDialer) dial(id peer.ID) error {
	ctx, cancel := context.WithTimeout(d.r.ctx, 45*time.Second)
	defer cancel()
	addrs := d.r.host.Peerstore().Addrs(id)
	if len(addrs) == 0 && d.r.dht != nil {
		if found, err := d.r.dht.FindPeer(ctx, id); err == nil {
			addrs = append(addrs, found.Addrs...)
		}
	}
	for _, relay := range d.relays {
		if relay.ID == id {
			continue
		}
		for _, ra := range relay.Addrs {
			circuit, err := ma.NewMultiaddr(fmt.Sprintf("%s/p2p/%s/p2p-circuit/p2p/%s", ra, relay.ID, id))
			if err == nil {
				addrs = append(addrs, circuit)
			}
		}
	}
	if len(addrs) == 0 {
		return fmt.Errorf("no addresses")
	}
	if err := d.r.host.Connect(ctx, peer.AddrInfo{ID: id, Addrs: addrs}); err != nil {
		return err
	}
	var via []string
	for _, c := range d.r.host.Network().ConnsToPeer(id) {
		via = append(via, c.RemoteMultiaddr().String())
	}
	d.r.log.Info("connected to roost", "peer", id, "via", via)
	return nil
}
