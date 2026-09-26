package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jancona/qtc/qtcd"
	"gopkg.in/ini.v1"
)

// The config file is INI, like m17-gateway's: sections and keys are
// case-insensitive, lists are comma-separated, and ';' or '#' start a
// comment line. Every section and key is checked, so a typo is an error
// rather than a silently ignored setting. See config.example.ini.
//
//	[General]  Callsign, DataDir, KeyFile, Admin, Software, MetricsInterval,
//	           Devices, Rooms, EchoRoomMessages
//	[Network]  Listen, Bootstrap, Caps, DHT, MailboxMembers, K
//	[Timers]   PresenceInterval, SweepInterval, RecordRefresh,
//	           TakeoverPeriod, MemberFailSilence, MemberFailSweeps
//	[Inet]     Listen, HostsFile, Gateways, AllowCallsigns
//	[Module X] Reflector, Module, Mode   (one per module letter X)
//
// The client face runs when there is an [Inet] section or any [Module X].
var configKeys = map[string][]string{
	"general": {"callsign", "datadir", "keyfile", "admin", "software", "metricsinterval", "devices", "rooms", "echoroommessages"},
	"network": {"listen", "bootstrap", "caps", "dht", "mailboxmembers", "k"},
	"timers":  {"presenceinterval", "sweepinterval", "recordrefresh", "takeoverperiod", "memberfailsilence", "memberfailsweeps"},
	"inet":    {"listen", "hostsfile", "gateways", "allowcallsigns"},
	"module":  {"reflector", "module", "mode"},
}

// fileConfig is a loaded config file: the station config plus the settings
// that belong to the binary rather than the station.
type fileConfig struct {
	qtcd.Config
	Admin string // loopback host:port for the admin HTTP interface; "" disables
}

func loadConfig(path string) (fileConfig, error) {
	f, err := ini.LoadSources(ini.LoadOptions{
		Insensitive:         true,
		KeyValueDelimiters:  "=",
		IgnoreInlineComment: true, // values never carry comments; keeps '#' literal
	}, path)
	if err != nil {
		return fileConfig{}, err
	}
	fc, err := parseConfig(f)
	if err != nil {
		return fileConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return fc, nil
}

func parseConfig(f *ini.File) (fileConfig, error) {
	if err := checkKeys(f); err != nil {
		return fileConfig{}, err
	}
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	gen, nw, tm := f.Section("general"), f.Section("network"), f.Section("timers")
	fc := fileConfig{Admin: str(gen, "admin")}
	cfg := &fc.Config
	cfg.Callsign = str(gen, "callsign")
	if cfg.Callsign == "" {
		fail("[General] Callsign is required")
	}
	cfg.DataDir = str(gen, "datadir")
	cfg.KeyFile = str(gen, "keyfile")
	cfg.Software = str(gen, "software")
	if cfg.Software == "" {
		cfg.Software = "qtcd/" + version
	}
	cfg.Devices = list(gen, "devices")
	cfg.Rooms = list(gen, "rooms")
	cfg.EchoRoomMessages = boolean(gen, "echoroommessages", false, fail)

	cfg.ListenAddrs = list(nw, "listen")
	if len(cfg.ListenAddrs) == 0 {
		cfg.ListenAddrs = []string{"/ip4/0.0.0.0/tcp/0"}
	}
	cfg.Bootstrap = list(nw, "bootstrap")
	cfg.EnableDHT = boolean(nw, "dht", true, fail)
	cfg.MailboxMembers = list(nw, "mailboxmembers")
	if k := integer(nw, "k", fail); k < 0 || k > 255 {
		fail("[Network] K must be 0 to 255")
	} else {
		cfg.K = uint8(k)
	}
	for _, c := range list(nw, "caps") {
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
			fail("[Network] Caps: unknown capability %q (public, relay, mailbox, clients)", c)
		}
	}

	for _, d := range []struct {
		sec *ini.Section
		key string
		dst *time.Duration
	}{
		{gen, "metricsinterval", &cfg.MetricsInterval},
		{tm, "presenceinterval", &cfg.PresenceInterval},
		{tm, "sweepinterval", &cfg.SweepInterval},
		{tm, "recordrefresh", &cfg.RecordRefresh},
		{tm, "takeoverperiod", &cfg.TakeoverPeriod},
		{tm, "memberfailsilence", &cfg.MemberFailSilence},
	} {
		if s := str(d.sec, d.key); s != "" {
			v, err := time.ParseDuration(s)
			if err != nil {
				fail("[%s] %s: %q is not a duration like 60s, 5m or 24h", title(d.sec), keyName(d.key), s)
				continue
			}
			*d.dst = v
		}
	}
	cfg.MemberFailSweeps = integer(tm, "memberfailsweeps", fail)

	modules := moduleSections(f)
	if f.HasSection("inet") || len(modules) > 0 {
		in := f.Section("inet")
		ic := &qtcd.InetConfig{
			Listen:         str(in, "listen"),
			HostsFile:      str(in, "hostsfile"),
			AllowCallsigns: list(in, "allowcallsigns"),
			Modules:        map[byte]qtcd.ModuleConfig{},
		}
		if ic.Listen == "" {
			ic.Listen = "0.0.0.0:17000"
		}
		for _, c := range list(in, "gateways") {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				fail("[Inet] Gateways: %q is not a CIDR range like 192.168.0.0/16", c)
				continue
			}
			ic.Gateways = append(ic.Gateways, n)
		}
		if len(modules) == 0 {
			fail("[Inet] needs at least one [Module X] section")
		}
		for letter, sec := range modules {
			m := strings.ToUpper(str(sec, "module"))
			mode := strings.ToLower(str(sec, "mode"))
			reflector := str(sec, "reflector")
			switch {
			case reflector == "":
				fail("[%s] Reflector is required", title(sec))
			case len(m) != 1 || m[0] < 'A' || m[0] > 'Z':
				fail("[%s] Module must be one letter A-Z", title(sec))
			case mode != string(qtcd.ModeQTC) && mode != string(qtcd.ModeNative):
				fail("[%s] Mode must be qtc or native", title(sec))
			default:
				ic.Modules[letter] = qtcd.ModuleConfig{Reflector: reflector, Module: m[0], Mode: qtcd.ModuleMode(mode)}
			}
		}
		cfg.Inet = ic
	}
	if len(errs) > 0 {
		return fc, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return fc, nil
}

// checkKeys rejects unknown sections and keys, and keys outside a section.
func checkKeys(f *ini.File) error {
	var errs []string
	for _, sec := range f.Sections() {
		name := sec.Name()
		kind := name
		if strings.HasPrefix(name, "module ") {
			kind = "module"
			if l := strings.TrimSpace(strings.TrimPrefix(name, "module ")); len(l) != 1 || l[0] < 'a' || l[0] > 'z' {
				errs = append(errs, fmt.Sprintf("[%s]: module sections are named [Module A] through [Module Z]", sec.Name()))
				continue
			}
		}
		if strings.EqualFold(name, ini.DefaultSection) {
			for _, k := range sec.Keys() {
				errs = append(errs, fmt.Sprintf("%s: every setting belongs in a section such as [General]", k.Name()))
			}
			continue
		}
		allowed, ok := configKeys[kind]
		if !ok {
			errs = append(errs, fmt.Sprintf("unknown section [%s] (General, Network, Timers, Inet, Module X)", name))
			continue
		}
		for _, k := range sec.Keys() {
			known := false
			for _, a := range allowed {
				known = known || k.Name() == a
			}
			if !known {
				errs = append(errs, fmt.Sprintf("[%s]: unknown setting %s", title(sec), k.Name()))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// moduleSections maps each [Module X] section to its upper-case letter.
func moduleSections(f *ini.File) map[byte]*ini.Section {
	out := map[byte]*ini.Section{}
	for _, sec := range f.Sections() {
		if l, ok := strings.CutPrefix(sec.Name(), "module "); ok {
			out[strings.ToUpper(strings.TrimSpace(l))[0]] = sec
		}
	}
	return out
}

func str(sec *ini.Section, key string) string {
	return strings.TrimSpace(sec.Key(key).String())
}

// list splits a comma-separated value, dropping empty items.
func list(sec *ini.Section, key string) []string {
	var out []string
	for _, s := range strings.Split(str(sec, key), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func boolean(sec *ini.Section, key string, def bool, fail func(string, ...any)) bool {
	s := str(sec, key)
	if s == "" {
		return def
	}
	switch strings.ToLower(s) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	fail("[%s] %s: %q is not true or false", title(sec), keyName(key), s)
	return def
}

func integer(sec *ini.Section, key string, fail func(string, ...any)) int {
	s := str(sec, key)
	if s == "" {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || fmt.Sprint(n) != s {
		fail("[%s] %s: %q is not a whole number", title(sec), keyName(key), s)
		return 0
	}
	return n
}

// title and keyName restore the documented spelling for error messages;
// the parser has folded everything to lower case.
func title(sec *ini.Section) string {
	if l, ok := strings.CutPrefix(sec.Name(), "module "); ok {
		return "Module " + strings.ToUpper(strings.TrimSpace(l))
	}
	return strings.ToUpper(sec.Name()[:1]) + sec.Name()[1:]
}

var keyNames = map[string]string{}

func init() {
	for _, k := range []string{"Callsign", "DataDir", "KeyFile", "Admin", "Software", "MetricsInterval", "Devices", "Rooms", "EchoRoomMessages",
		"Listen", "Bootstrap", "Caps", "DHT", "MailboxMembers", "K", "PresenceInterval", "SweepInterval", "RecordRefresh",
		"TakeoverPeriod", "MemberFailSilence", "MemberFailSweeps", "HostsFile", "Gateways", "AllowCallsigns", "Reflector", "Module", "Mode"} {
		keyNames[strings.ToLower(k)] = k
	}
}

func keyName(key string) string {
	if n, ok := keyNames[key]; ok {
		return n
	}
	return key
}
