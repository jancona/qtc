// Command roost runs a Pigeon node.
//
// Usage: roost -config roost.json [-log-level debug|info|warn]
//
// The config file is JSON; see config.example.json alongside this source.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jancona/pigeon/roost"
)

// fileConfig is the on-disk shape of roost.Config.
type fileConfig struct {
	Callsign         string   `json:"callsign"`
	KeyFile          string   `json:"key_file"`
	Listen           []string `json:"listen"`
	Bootstrap        []string `json:"bootstrap"`
	Caps             []string `json:"caps"` // public, relay, inbox, clients
	Software         string   `json:"software"`
	DHT              bool     `json:"dht"`
	InboxMembers     []string `json:"inbox_members"`
	Rooms            []string `json:"rooms"`
	Devices          []string `json:"devices"`
	MetricsInterval  string   `json:"metrics_interval"` // Go duration, e.g. "60s"; "" disables
	Admin            string   `json:"admin"`            // loopback host:port for the admin HTTP interface; "" disables
	PresenceInterval string   `json:"presence_interval"`
	SweepInterval    string   `json:"sweep_interval"`
}

func main() {
	var (
		path     = flag.String("config", "roost.json", "config file")
		logLevel = flag.String("log-level", "info", "debug, info, warn, or error")
		printID  = flag.Bool("print-id", false, "load or create the node key, print the peer ID, and exit")
	)
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "roost: bad log level %q\n", *logLevel)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	cfg, err := loadConfig(*path)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	cfg.Log = log

	r, err := roost.New(cfg)
	if err != nil {
		log.Error("new roost", "err", err)
		os.Exit(1)
	}
	if *printID {
		id, err := r.PeerID()
		if err != nil {
			log.Error("peer id", "err", err)
			os.Exit(1)
		}
		fmt.Println(id)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := r.Start(ctx); err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}
	for _, a := range r.AddrInfo().Addrs {
		fmt.Printf("%s/p2p/%s\n", a, r.ID())
	}
	if adminAddr != "" {
		if _, err := r.ServeAdmin(adminAddr); err != nil {
			log.Error("admin", "err", err)
			os.Exit(1)
		}
	}
	<-ctx.Done()
	log.Info("shutting down")
	if err := r.Stop(); err != nil {
		log.Warn("stop", "err", err)
	}
}

// adminAddr is set by loadConfig.
var adminAddr string

func loadConfig(path string) (roost.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return roost.Config{}, err
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return roost.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	adminAddr = fc.Admin
	cfg := roost.Config{
		Callsign:     fc.Callsign,
		KeyFile:      fc.KeyFile,
		ListenAddrs:  fc.Listen,
		Bootstrap:    fc.Bootstrap,
		Software:     fc.Software,
		EnableDHT:    fc.DHT,
		InboxMembers: fc.InboxMembers,
		Rooms:        fc.Rooms,
		Devices:      fc.Devices,
	}
	for _, c := range fc.Caps {
		switch strings.ToLower(c) {
		case "public":
			cfg.Caps |= roost.CapPublic
		case "relay":
			cfg.Caps |= roost.CapRelay
		case "inbox":
			cfg.Caps |= roost.CapInbox
		case "clients":
			cfg.Caps |= roost.CapClients
		default:
			return cfg, fmt.Errorf("unknown capability %q", c)
		}
	}
	for _, d := range []struct {
		s   string
		dst *time.Duration
	}{{fc.MetricsInterval, &cfg.MetricsInterval}, {fc.PresenceInterval, &cfg.PresenceInterval}, {fc.SweepInterval, &cfg.SweepInterval}} {
		if d.s == "" {
			continue
		}
		v, err := time.ParseDuration(d.s)
		if err != nil {
			return cfg, fmt.Errorf("bad duration %q: %w", d.s, err)
		}
		*d.dst = v
	}
	if len(cfg.ListenAddrs) == 0 {
		cfg.ListenAddrs = []string{"/ip4/0.0.0.0/tcp/0"}
	}
	return cfg, nil
}
