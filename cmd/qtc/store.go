package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
)

// runStore talks the storage protocol to a mailbox node:
//
//	qtc store put   -peer ADDR -callsign CALL <envelope hex|base64|->
//	qtc store query -peer ADDR -callsign CALL [-since T] [-types MSG,RCPT,ROOM]
//	qtc store watch -peer ADDR -callsign CALL[,CALL...]
func runStore(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: qtc store put|query|watch -peer <multiaddr> -callsign CALL [args]")
	}
	op := args[0]
	fs := flag.NewFlagSet("store "+op, flag.ContinueOnError)
	peerAddr := fs.String("peer", "", "mailbox node multiaddr with /p2p/<ID>")
	callsign := fs.String("callsign", "", "callsign (or #ROOM); watch accepts a comma-separated list")
	since := fs.Uint("since", 0, "query: received-at time to start from (Unix seconds)")
	types := fs.String("types", "MSG,RCPT,ROOM", "query: envelope types to fetch")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *peerAddr == "" || *callsign == "" {
		return errors.New("-peer and -callsign are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cl, closeFn, err := dialStore(ctx, *peerAddr)
	if err != nil {
		return err
	}
	defer closeFn()

	switch op {
	case "put":
		if fs.NArg() != 1 {
			return errors.New("put needs one envelope argument")
		}
		raw, err := readEnvelopeArg(fs.Arg(0))
		if err != nil {
			return err
		}
		e, err := envelope.Parse(raw)
		if err != nil {
			return err
		}
		a, err := envelope.ParseAddress(*callsign)
		if err != nil {
			return err
		}
		if err := cl.Put(ctx, a, e); err != nil {
			return err
		}
		fmt.Printf("stored %s under %s\n", e.StoreID(), a.Base())
		return nil
	case "query":
		a, err := envelope.ParseAddress(*callsign)
		if err != nil {
			return err
		}
		var ts []envelope.PacketType
		for _, t := range strings.Split(*types, ",") {
			switch strings.ToUpper(strings.TrimSpace(t)) {
			case "MSG":
				ts = append(ts, envelope.TypeMSG)
			case "RCPT":
				ts = append(ts, envelope.TypeRCPT)
			case "ROOM":
				ts = append(ts, envelope.TypeROOM)
			default:
				return fmt.Errorf("unknown type %q", t)
			}
		}
		envs, err := cl.QueryAll(ctx, a, uint32(*since), ts)
		if err != nil {
			return err
		}
		for _, e := range envs {
			fmt.Printf("%s\t%s\n", base64.StdEncoding.EncodeToString(e.Bytes()), e)
		}
		fmt.Fprintf(os.Stderr, "%d envelopes\n", len(envs))
		return nil
	case "watch":
		var calls []envelope.Address
		for _, c := range strings.Split(*callsign, ",") {
			a, err := envelope.ParseAddress(strings.TrimSpace(c))
			if err != nil {
				return err
			}
			calls = append(calls, a)
		}
		if err := cl.Watch(calls...); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "watching; Ctrl-C to stop")
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-cl.Done():
				return fmt.Errorf("stream closed: %v", cl.Err())
			case ev, ok := <-cl.Events():
				if !ok {
					return nil
				}
				fmt.Printf("%s\t%s\t%s\n", ev.Callsign, base64.StdEncoding.EncodeToString(ev.Env.Bytes()), ev.Env)
			}
		}
	}
	return fmt.Errorf("unknown store subcommand %q", op)
}

// dialStore opens an ephemeral libp2p host, connects to the mailbox node,
// and returns a store client on a fresh stream.
func dialStore(ctx context.Context, addr string) (*store.Client, func(), error) {
	m, err := ma.NewMultiaddr(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("peer: %w", err)
	}
	ai, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil {
		return nil, nil, fmt.Errorf("peer: %w", err)
	}
	h, err := libp2p.New(libp2p.NoListenAddrs, libp2p.Transport(tcp.NewTCPTransport), libp2p.Security(libp2ptls.ID, libp2ptls.New), libp2p.UserAgent("qtc-cli"))
	if err != nil {
		return nil, nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := h.Connect(dctx, *ai); err != nil {
		h.Close()
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	s, err := h.NewStream(dctx, ai.ID, store.ProtocolID)
	if err != nil {
		h.Close()
		return nil, nil, fmt.Errorf("open store stream: %w", err)
	}
	cl := store.NewClient(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return cl, func() { s.Close(); h.Close() }, nil
}
