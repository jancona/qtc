package qtcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
)

// Topic and protocol names, version 0.
const (
	PresenceTopic   = "/qtc/0/presence"
	RoomTopicPrefix = "/qtc/0/room/"
)

// startP2P builds the libp2p host, DHT, gossipsub, and the store server.
func (r *Station) startP2P() error {
	ident, err := libp2pKey(r.key)
	if err != nil {
		return err
	}
	bootstrap, err := parseAddrInfos(r.cfg.Bootstrap)
	if err != nil {
		return err
	}
	opts := []libp2p.Option{
		libp2p.Identity(ident),
		libp2p.ListenAddrStrings(r.cfg.ListenAddrs...),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.UserAgent(r.cfg.Software),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(),
	}
	if r.cfg.Caps.Has(CapPublic) {
		opts = append(opts, libp2p.ForceReachabilityPublic())
	} else {
		// A station without the public capability is assumed to be behind
		// NAT: it reserves circuit relay slots on its bootstrap peers right away
		// and hole-punches from there.
		opts = append(opts, libp2p.ForceReachabilityPrivate())
		if len(bootstrap) > 0 {
			opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(bootstrap))
		}
	}
	if r.cfg.Caps.Has(CapRelay) {
		// The spike carries all traffic over the circuit relay when hole punching
		// fails, so the default per-connection limits are lifted.
		opts = append(opts, libp2p.EnableRelayService(relay.WithInfiniteLimits()))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return fmt.Errorf("qtcd: libp2p host: %w", err)
	}
	r.host = h

	if r.cfg.EnableDHT {
		mode := dht.ModeClient
		if r.cfg.Caps.Has(CapPublic) {
			mode = dht.ModeServer
		}
		d, err := dht.New(h, dht.Mode(mode), dht.BootstrapPeers(bootstrap...), dht.ProtocolPrefix("/qtc"),
			dht.NamespacedValidator("qtc", recordValidator{}))
		if err != nil {
			return fmt.Errorf("qtcd: dht: %w", err)
		}
		r.dht = d
	}

	ps, err := pubsub.NewGossipSub(r.ctx, h,
		pubsub.WithMessageSignaturePolicy(pubsub.StrictSign),
		pubsub.WithMessageIdFn(func(m *pb.Message) string {
			sum := sha256.Sum256(m.Data)
			return hex.EncodeToString(sum[:16])
		}),
	)
	if err != nil {
		return fmt.Errorf("qtcd: gossipsub: %w", err)
	}
	r.ps = ps

	if r.cfg.Caps.Has(CapMailbox) {
		r.mem = store.NewMemStore()
		r.server = store.NewServer(r.mem)
		r.server.Policy = store.Policy{DefaultTTL: r.cfg.DefaultTTL}
		r.server.Log = r.log
		r.server.OnStored = func(c envelope.Address, e *envelope.Envelope) { r.onStored(c, e, h.ID()) }
		h.SetStreamHandler(store.ProtocolID, func(s network.Stream) {
			defer s.Close()
			if err := r.server.Serve(s); err != nil {
				r.log.Debug("store stream ended", "peer", s.Conn().RemotePeer(), "err", err)
			}
		})
	}

	r.dialer = newPeerDialer(r, bootstrap)
	for _, ai := range bootstrap {
		ai := ai
		r.go_(func() { r.connectLoop(ai) })
	}
	if r.dht != nil {
		if err := r.dht.Bootstrap(r.ctx); err != nil {
			r.log.Warn("dht bootstrap", "err", err)
		}
	}
	return nil
}

// connectLoop keeps a connection to a bootstrap peer.
func (r *Station) connectLoop(ai peer.AddrInfo) {
	delay := time.Second
	for {
		if r.host.Network().Connectedness(ai.ID) != network.Connected {
			ctx, cancel := context.WithTimeout(r.ctx, 20*time.Second)
			err := r.host.Connect(ctx, ai)
			cancel()
			if err != nil {
				r.log.Warn("bootstrap connect failed", "peer", ai.ID, "err", err)
				delay = min(delay*2, time.Minute)
			} else {
				r.log.Info("connected to bootstrap peer", "peer", ai.ID)
				delay = time.Second
			}
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(delay + 5*time.Second):
		}
	}
}

// Connect dials another node directly. Tests use it to build a mesh.
func (r *Station) Connect(ctx context.Context, ai peer.AddrInfo) error {
	return r.host.Connect(ctx, ai)
}

func parseAddrInfos(addrs []string) ([]peer.AddrInfo, error) {
	byID := map[peer.ID]*peer.AddrInfo{}
	var order []peer.ID
	for _, s := range addrs {
		m, err := ma.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("qtcd: bootstrap %q: %w", s, err)
		}
		ai, err := peer.AddrInfoFromP2pAddr(m)
		if err != nil {
			return nil, fmt.Errorf("qtcd: bootstrap %q: %w", s, err)
		}
		if cur := byID[ai.ID]; cur != nil {
			cur.Addrs = append(cur.Addrs, ai.Addrs...)
			continue
		}
		byID[ai.ID] = ai
		order = append(order, ai.ID)
	}
	out := make([]peer.AddrInfo, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// openStore opens a store protocol stream to a mailbox node.
func (r *Station) openStore(ctx context.Context, id peer.ID) (network.Stream, error) {
	s, err := r.host.NewStream(ctx, id, store.ProtocolID)
	if err != nil {
		return nil, fmt.Errorf("qtcd: open store stream to %s: %w", id, err)
	}
	return s, nil
}
