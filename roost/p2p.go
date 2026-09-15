package roost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jancona/pigeon/store"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
)

// Topic and protocol names, version 0.
const (
	PresenceTopic   = "/pigeon/0/presence"
	RoomTopicPrefix = "/pigeon/0/room/"
)

// startP2P builds the libp2p host, DHT, gossipsub, and the store server.
func (r *Roost) startP2P() error {
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
	} else if len(bootstrap) > 0 {
		opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(bootstrap))
	}
	if r.cfg.Caps.Has(CapRelay) {
		opts = append(opts, libp2p.EnableRelayService())
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return fmt.Errorf("roost: libp2p host: %w", err)
	}
	r.host = h

	if r.cfg.EnableDHT {
		mode := dht.ModeClient
		if r.cfg.Caps.Has(CapPublic) {
			mode = dht.ModeServer
		}
		d, err := dht.New(h, dht.Mode(mode), dht.BootstrapPeers(bootstrap...), dht.ProtocolPrefix("/pigeon"))
		if err != nil {
			return fmt.Errorf("roost: dht: %w", err)
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
		return fmt.Errorf("roost: gossipsub: %w", err)
	}
	r.ps = ps

	if r.cfg.Caps.Has(CapInbox) {
		r.mem = store.NewMemStore()
		r.server = store.NewServer(r.mem)
		r.server.Policy = store.Policy{DefaultTTL: r.cfg.DefaultTTL}
		r.server.Log = r.log
		h.SetStreamHandler(store.ProtocolID, func(s network.Stream) {
			defer s.Close()
			if err := r.server.Serve(s); err != nil {
				r.log.Debug("store stream ended", "peer", s.Conn().RemotePeer(), "err", err)
			}
		})
	}

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
func (r *Roost) connectLoop(ai peer.AddrInfo) {
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
func (r *Roost) Connect(ctx context.Context, ai peer.AddrInfo) error {
	return r.host.Connect(ctx, ai)
}

func parseAddrInfos(addrs []string) ([]peer.AddrInfo, error) {
	byID := map[peer.ID]*peer.AddrInfo{}
	var order []peer.ID
	for _, s := range addrs {
		m, err := ma.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("roost: bootstrap %q: %w", s, err)
		}
		ai, err := peer.AddrInfoFromP2pAddr(m)
		if err != nil {
			return nil, fmt.Errorf("roost: bootstrap %q: %w", s, err)
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

// openStore opens a store protocol stream to an inbox node.
func (r *Roost) openStore(ctx context.Context, id peer.ID) (network.Stream, error) {
	s, err := r.host.NewStream(ctx, id, store.ProtocolID)
	if err != nil {
		return nil, fmt.Errorf("roost: open store stream to %s: %w", id, err)
	}
	return s, nil
}
