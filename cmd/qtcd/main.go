// Command qtcd runs a QTC node.
//
// Usage: qtcd -config qtcd.ini [-log-level debug|info|warn] [-print-id] [-version]
//
// The config file is INI; see config.go and config.example.ini alongside
// this source.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jancona/qtc/qtcd"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		path     = flag.String("config", "qtcd.ini", "config file")
		logLevel = flag.String("log-level", "info", "debug, info, warn, or error")
		printID  = flag.Bool("print-id", false, "load or create the node key, print the peer ID, and exit")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "qtcd: bad log level %q\n", *logLevel)
		os.Exit(2)
	}
	journal := underJournal()
	log := newLogger(os.Stderr, level, journal)
	slog.SetDefault(log)

	fc, err := loadConfig(*path)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	fc.Log = log

	r, err := qtcd.New(fc.Config)
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
	if !journal {
		// For copying into another node's bootstrap list; under systemd the
		// "station started" log line carries the same addresses.
		for _, a := range r.AddrInfo().Addrs {
			fmt.Printf("%s/p2p/%s\n", a, r.ID())
		}
	}
	if fc.Admin != "" {
		if _, err := r.ServeAdmin(fc.Admin); err != nil {
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
