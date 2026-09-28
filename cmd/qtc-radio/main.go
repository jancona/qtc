// Command qtc-radio is a native QTC radio made from a hotspot modem: it
// transmits and receives the QTC packet type over RF the way a radio with
// QTC firmware would (docs/qtc-client.md), for testing hotspots and nodes
// without one. It reads the modem and radio settings from an m17-gateway
// config file, so a spare hotspot becomes a test radio; its frequencies
// should be the channel of the hotspot under test.
//
// Usage:
//
//	qtc-radio -config m17-gateway.ini -callsign "N1ADJ 7" [-drop 0.1] [-state FILE] [-log-level debug]
//
// It reads typed lines like qtc chat (@CALL text, #ROOM text, /join, /sync,
// /quit) and prints what it receives. Pipe lines in to script a test.
// -drop discards that fraction of received packets, to exercise retries.
//
// The modem must not be in use by m17-gateway at the same time.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/m17/modem"
	"github.com/jancona/qtc/client"
	"github.com/jancona/qtc/envelope"
	"gopkg.in/ini.v1"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "qtc-radio:", err)
		os.Exit(1)
	}
}

// radioConfig is what qtc-radio takes from an m17-gateway config file.
type radioConfig struct {
	rxFrequency, txFrequency uint32
	power                    float64
	afc, duplex              bool
	frequencyCorr            int16
	rxHoldoff, packetGap     time.Duration
	modemType                string
	modemCfg                 *ini.Section
}

func loadRadioConfig(path string) (radioConfig, error) {
	f, err := ini.Load(path)
	if err != nil {
		return radioConfig{}, err
	}
	r := f.Section("Radio")
	var c radioConfig
	rx, err1 := r.Key("RXFrequency").Uint()
	tx, err2 := r.Key("TXFrequency").Uint()
	corr, err3 := r.Key("FrequencyCorr").Int()
	c.power = r.Key("Power").MustFloat64(10)
	c.afc = r.Key("AFC").MustBool(false)
	c.duplex = r.Key("Duplex").MustBool(false)
	c.rxHoldoff = r.Key("RXHoldoff").MustDuration(500 * time.Millisecond)
	c.packetGap = r.Key("PacketGap").MustDuration(250 * time.Millisecond)
	c.modemCfg = f.Section("Modem")
	c.modemType = strings.ToLower(c.modemCfg.Key("Type").MustString("cc1200"))
	if err := errors.Join(err1, err2, err3); err != nil {
		return c, fmt.Errorf("%s [Radio]: %w", path, err)
	}
	c.rxFrequency, c.txFrequency, c.frequencyCorr = uint32(rx), uint32(tx), int16(corr)
	return c, nil
}

func openModem(c radioConfig) (modem.Modem, error) {
	switch c.modemType {
	case "cc1200", "cc1200v2":
		return modem.NewCC1200(c.rxFrequency, c.txFrequency, int8(c.power), c.frequencyCorr, c.afc, c.modemCfg)
	case "mmdvm":
		return modem.NewMMDVM(c.rxFrequency, c.txFrequency, float32(c.power), c.frequencyCorr, c.afc, c.modemCfg, c.duplex)
	case "sx1255":
		return modem.NewSX1255(c.rxFrequency, c.txFrequency, c.frequencyCorr, c.modemCfg)
	}
	return nil, fmt.Errorf("modem type %q is not cc1200, mmdvm, or sx1255", c.modemType)
}

func run() error {
	configPath := flag.String("config", "/etc/m17-gateway.ini", "m17-gateway config with this modem's [Radio] and [Modem] settings")
	callsign := flag.String("callsign", "", `this radio's callsign, e.g. "N1ADJ 7" (required)`)
	drop := flag.Float64("drop", 0, "fraction of received packets to discard, 0 to 1")
	statePath := flag.String("state", "", "file keeping the sync position (default: per callsign in the user config directory)")
	logLevel := flag.String("log-level", "info", "debug, info, or warn")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	me, err := envelope.EncodeAddress(strings.ToUpper(strings.TrimSpace(*callsign)))
	if err != nil || !me.IsStandard() {
		return fmt.Errorf("-callsign %q is not a callsign", *callsign)
	}
	if *drop < 0 || *drop > 1 {
		return errors.New("-drop must be between 0 and 1")
	}
	cfg, err := loadRadioConfig(*configPath)
	if err != nil {
		return err
	}
	mdm, err := openModem(cfg)
	if err != nil {
		return fmt.Errorf("modem: %w", err)
	}
	defer mdm.Close()
	if *statePath == "" {
		*statePath = client.DefaultStatePath(me)
	}

	r := &radio{log: log, modem: mdm, me: me, drop: *drop, rxHoldoff: cfg.rxHoldoff, packetGap: cfg.packetGap, queue: make(chan m17.Packet, 64)}
	out := func(line string) { fmt.Println(line) }
	term := &client.Terminal{Out: out}
	r.sess = client.New(client.Config{Me: me, RF: true, StatePath: *statePath, Send: r.send, Event: term.Show})
	term.Sess = r.sess

	if err := mdm.Start(); err != nil {
		return fmt.Errorf("modem start: %w", err)
	}
	d := m17.NewDecoder(r.rxLSF, r.rxStream, r.rxLICH, r.rxEOT, r.rxPacket)
	mdm.StartDecoding(d.DecodeFrame)
	go r.transmit()
	log.Info("radio on the air", "callsign", me, "modem", cfg.modemType, "rx", cfg.rxFrequency, "tx", cfg.txFrequency, "drop", *drop)
	out(fmt.Sprintf("qtc-radio %s as %s. Syncing… (/help for help)", version, me))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r.sess.Linked() // a radio is always "linked": the channel is its link
	r.sess.RoomRequest(envelope.OpList, nil)

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.report()
			return nil
		case now := <-tick.C:
			r.sess.Tick(now)
		case line, ok := <-lines:
			if !ok {
				// Input ended (a script finished): keep listening until
				// interrupted, so replies and receipts still arrive.
				lines = nil
				continue
			}
			if term.Input(line) {
				r.report()
				return nil
			}
		}
	}
}

// radio connects a client session to a modem.
type radio struct {
	log       *slog.Logger
	modem     modem.Modem
	sess      *client.Session
	me        envelope.Address
	drop      float64
	rxHoldoff time.Duration
	packetGap time.Duration
	queue     chan m17.Packet

	mu     sync.Mutex
	lastRX time.Time
	lastTX time.Time
	counts struct{ rx, dropped, tx, other int }
}

// heard notes a frame decoded from the receiver: the channel is busy.
func (r *radio) heard() {
	r.mu.Lock()
	r.lastRX = time.Now()
	r.mu.Unlock()
}

func (r *radio) rxLSF(m17.LSF, float64) error                            { r.heard(); return nil }
func (r *radio) rxStream(m17.LSF, []byte, uint16, uint16, float64) error { r.heard(); return nil }
func (r *radio) rxLICH(m17.LSF, float64) error                           { r.heard(); return nil }
func (r *radio) rxEOT(m17.LSF, uint16, uint16, float64) error            { r.heard(); return nil }

// rxPacket takes a received packet: QTC payloads go to the session,
// possibly after a deliberate drop.
func (r *radio) rxPacket(lsf m17.LSF, payload []byte, ber float64) error {
	r.heard()
	p := m17.NewPacketFromBytes(append(lsf.ToBytes(), payload...))
	dst, src := envelope.AddressFromBytes(lsf.Dst[:]), envelope.AddressFromBytes(lsf.Src[:])
	if byte(p.Type) != byte(envelope.TypeQTC) {
		r.count(func() { r.counts.other++ })
		if p.Type == m17.PacketTypeSMS && dst.Base() == r.me.Base() {
			text, _, _ := strings.Cut(string(p.Payload), "\x00")
			fmt.Printf("%s %s (SMS): %s\n", time.Now().Format("15:04"), src, text)
		}
		return nil
	}
	if r.drop > 0 && rand.Float64() < r.drop {
		r.count(func() { r.counts.dropped++ })
		r.log.Debug("dropped a received packet on purpose", "src", src, "dst", dst)
		return nil
	}
	e, err := envelope.Parse(append([]byte{byte(p.Type)}, p.Payload...))
	if err != nil {
		r.log.Debug("unparseable QTC packet", "src", src, "err", err)
		return nil
	}
	r.count(func() { r.counts.rx++ })
	r.log.Debug("received", "src", src, "dst", dst, "packet", e, "mer", ber)
	r.sess.Receive(dst, src, e)
	return nil
}

func (r *radio) count(f func()) {
	r.mu.Lock()
	f()
	r.mu.Unlock()
}

// send queues a QTC payload for transmission.
func (r *radio) send(dst envelope.Address, e *envelope.Envelope) {
	lsf, err := m17.NewLSF("N0CALL", "N0CALL", m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	if err != nil {
		r.log.Error("build LSF", "err", err)
		return
	}
	// Raw address bytes: m17's text encoding would read a room's "#NAME"
	// as an M17 hash address.
	lsf.Dst = m17.EncodedCallsign(dst.Bytes())
	lsf.Src = m17.EncodedCallsign(r.me.Bytes())
	lsf.CalcCRC()
	b := e.Bytes()
	p := m17.Packet{LSF: &lsf, Type: m17.PacketType(b[0]), Payload: b[1:]}
	p.CalcCRC()
	select {
	case r.queue <- p:
	default:
		r.log.Warn("transmit queue full; packet dropped", "packet", e)
	}
}

// transmit sends queued packets one at a time, each when the channel is
// clear: nothing received for rxHoldoff, and packetGap since our own last
// transmission, as m17-gateway does.
func (r *radio) transmit() {
	for p := range r.queue {
		for {
			r.mu.Lock()
			wait := max(time.Until(r.lastRX.Add(r.rxHoldoff)), time.Until(r.lastTX.Add(r.packetGap)))
			r.mu.Unlock()
			if wait <= 0 {
				break
			}
			time.Sleep(min(wait, 50*time.Millisecond))
		}
		if e, err := envelope.Parse(append([]byte{byte(p.Type)}, p.Payload...)); err == nil {
			r.log.Debug("transmitting", "dst", envelope.AddressFromBytes(p.LSF.Dst[:]), "packet", e)
		}
		if err := r.modem.TransmitPacket(p); err != nil {
			r.log.Error("transmit", "err", err)
		}
		r.mu.Lock()
		r.lastTX = time.Now()
		r.counts.tx++
		r.mu.Unlock()
	}
}

// report logs what went over the air, for test runs.
func (r *radio) report() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log.Info("packets", "received", r.counts.rx, "dropped_on_purpose", r.counts.dropped, "other", r.counts.other, "transmitted", r.counts.tx)
}
