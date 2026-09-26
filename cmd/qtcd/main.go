// Command qtcd runs a QTC node.
//
// Usage: qtcd -config qtcd.json [-log-level debug|info|warn]
//
// The config file is JSON; see config.example.json alongside this source.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jancona/qtc/qtcd"
)

// fileConfig is the on-disk shape of qtcd.Config.
type fileConfig struct {
	Callsign         string   `json:"callsign"`
	KeyFile          string   `json:"key_file"`
	Listen           []string `json:"listen"`
	Bootstrap        []string `json:"bootstrap"`
	Caps             []string `json:"caps"` // public, relay, mailbox, clients
	Software         string   `json:"software"`
	DHT              bool     `json:"dht"`
	MailboxMembers   []string `json:"mailbox_members"`
	K                int      `json:"k"` // mailbox target size, default 2
	Rooms            []string `json:"rooms"`
	Devices          []string `json:"devices"`
	MetricsInterval  string   `json:"metrics_interval"`   // Go duration, e.g. "60s"; "" disables
	EchoRoomMessages bool     `json:"echo_room_messages"` // deliver a room message back to its sender\'s device (testing)
	Admin            string   `json:"admin"`              // loopback host:port for the admin HTTP interface; "" disables
	Inet             *struct {
		Listen    string   `json:"listen"`     // UDP address the reflector face listens on, e.g. "0.0.0.0:17000"
		HostsFile string   `json:"hosts_file"` // M17Hosts.txt for resolving reflector names
		Gateways  []string `json:"gateways"`   // CIDRs whose clients are RF gateways; default private ranges
		Modules   map[string]struct {
			Reflector string `json:"reflector"` // upstream name from hosts_file, or host:port
			Module    string `json:"module"`    // upstream module letter
			Mode      string `json:"mode"`      // "native" or "qtc"
		} `json:"modules"`
	} `json:"inet"`
	PresenceInterval  string `json:"presence_interval"`
	SweepInterval     string `json:"sweep_interval"`
	RecordRefresh     string `json:"record_refresh"`
	TakeoverPeriod    string `json:"takeover_period"`
	MemberFailSilence string `json:"member_fail_silence"`
	MemberFailSweeps  int    `json:"member_fail_sweeps"`
}

func main() {
	var (
		path     = flag.String("config", "qtcd.json", "config file")
		logLevel = flag.String("log-level", "info", "debug, info, warn, or error")
		printID  = flag.Bool("print-id", false, "load or create the node key, print the peer ID, and exit")
	)
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "qtcd: bad log level %q\n", *logLevel)
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

	r, err := qtcd.New(cfg)
	if err != nil {
		log.Error("new station", "err", err)
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

func loadConfig(path string) (qtcd.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return qtcd.Config{}, err
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return qtcd.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	adminAddr = fc.Admin
	cfg := qtcd.Config{
		Callsign:         fc.Callsign,
		KeyFile:          fc.KeyFile,
		ListenAddrs:      fc.Listen,
		Bootstrap:        fc.Bootstrap,
		Software:         fc.Software,
		EnableDHT:        fc.DHT,
		MailboxMembers:   fc.MailboxMembers,
		K:                uint8(fc.K),
		MemberFailSweeps: fc.MemberFailSweeps,
		Rooms:            fc.Rooms,
		Devices:          fc.Devices,
		EchoRoomMessages: fc.EchoRoomMessages,
	}
	for _, c := range fc.Caps {
		switch strings.ToLower(c) {
		case "public":
			cfg.Caps |= qtcd.CapPublic
		case "relay":
			cfg.Caps |= qtcd.CapRelay
		case "mailbox":
			cfg.Caps |= qtcd.CapMailbox
		case "clients":
			cfg.Caps |= qtcd.CapClients
		default:
			return cfg, fmt.Errorf("unknown capability %q", c)
		}
	}
	for _, d := range []struct {
		s   string
		dst *time.Duration
	}{{fc.MetricsInterval, &cfg.MetricsInterval}, {fc.PresenceInterval, &cfg.PresenceInterval}, {fc.SweepInterval, &cfg.SweepInterval}, {fc.RecordRefresh, &cfg.RecordRefresh}, {fc.TakeoverPeriod, &cfg.TakeoverPeriod}, {fc.MemberFailSilence, &cfg.MemberFailSilence}} {
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
	if fc.Inet != nil {
		ic := &qtcd.InetConfig{Listen: fc.Inet.Listen, HostsFile: fc.Inet.HostsFile, Modules: map[byte]qtcd.ModuleConfig{}}
		if ic.Listen == "" {
			ic.Listen = "0.0.0.0:17000"
		}
		for _, c := range fc.Inet.Gateways {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				return cfg, fmt.Errorf("inet.gateways %q: %w", c, err)
			}
			ic.Gateways = append(ic.Gateways, n)
		}
		for letter, m := range fc.Inet.Modules {
			if len(letter) != 1 || len(m.Module) != 1 {
				return cfg, fmt.Errorf("inet.modules: module letters must be single characters (%q -> %q)", letter, m.Module)
			}
			ic.Modules[strings.ToUpper(letter)[0]] = qtcd.ModuleConfig{
				Reflector: m.Reflector,
				Module:    strings.ToUpper(m.Module)[0],
				Mode:      qtcd.ModuleMode(strings.ToLower(m.Mode)),
			}
		}
		cfg.Inet = ic
	}
	return cfg, nil
}
