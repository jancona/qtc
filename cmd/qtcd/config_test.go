package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jancona/qtc/qtcd"
)

func writeConfig(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qtcd.ini")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExampleConfig(t *testing.T) {
	fc, err := loadConfig("config.example.ini")
	if err != nil {
		t.Fatal(err)
	}
	c := fc.Config
	if c.Callsign != "N1ADJ   Q" {
		t.Errorf("Callsign = %q; internal spaces must survive", c.Callsign)
	}
	if fc.Admin != "127.0.0.1:8017" || c.DataDir != "/var/lib/qtcd" || c.KeyFile != "" {
		t.Errorf("General = %+v admin %q", c, fc.Admin)
	}
	if !c.EnableDHT || c.K != 2 || c.Caps != 0 || len(c.Bootstrap) != 1 || !strings.HasPrefix(c.Bootstrap[0], "/dns4/ham.n1adj.net/") {
		t.Errorf("Network: dht %v k %d caps %v bootstrap %v", c.EnableDHT, c.K, c.Caps, c.Bootstrap)
	}
	if c.MetricsInterval != 15*time.Minute || c.TakeoverPeriod != 168*time.Hour || c.MemberFailSweeps != 3 {
		t.Errorf("durations: metrics %v takeover %v sweeps %d", c.MetricsInterval, c.TakeoverPeriod, c.MemberFailSweeps)
	}
	if c.Software == "" {
		t.Error("Software not defaulted")
	}
	if c.ReachWindow != time.Hour || c.ReplayLimit != 10 {
		t.Errorf("Delivery: reach %v limit %d", c.ReachWindow, c.ReplayLimit)
	}
	if c.Inet == nil {
		t.Fatal("no client face")
	}
	want := qtcd.ModuleConfig{Reflector: "M17-M17", Module: 'T', Mode: qtcd.ModeQTC}
	if c.Inet.Listen != "127.0.0.1:17000" || len(c.Inet.Modules) != 1 || c.Inet.Modules['A'] != want {
		t.Errorf("Inet = %+v", c.Inet)
	}
}

func TestLoadPackageSample(t *testing.T) {
	b, err := os.ReadFile("packaging/qtcd.ini.sample")
	if err != nil {
		t.Fatal(err)
	}
	// As postinst fills it in.
	s := strings.NewReplacer("CALLSIGN_PLACEHOLDER", "N1ADJ   Q", "HOSTS_FILE_PLACEHOLDER", "/opt/m17/rpi-dashboard/files/M17Hosts.txt",
		"REFLECTOR_PLACEHOLDER", "M17-M17", "MODULE_PLACEHOLDER", "T").Replace(string(b))
	fc, err := loadConfig(writeConfig(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Callsign != "N1ADJ   Q" || fc.Inet == nil || fc.Inet.Modules['A'].Reflector != "M17-M17" || !fc.EnableDHT {
		t.Errorf("sample loaded as %+v", fc.Config)
	}
}

func TestConfigListsAndModules(t *testing.T) {
	fc, err := loadConfig(writeConfig(t, `
[general]
callsign = N1ADJ  P
[NETWORK]
Listen = /ip4/0.0.0.0/tcp/4001 , /ip6/::/tcp/4001
Caps = public, Relay,mailbox
DHT = no
[Inet]
Listen = 0.0.0.0:17000
Gateways = 10.0.0.0/8, 192.168.0.0/16
AllowCallsigns = N1ADJ, W1AW
[module b]
Reflector = 203.0.113.5:17000
Module = c
Mode = Native
`))
	if err != nil {
		t.Fatal(err)
	}
	c := fc.Config
	if len(c.ListenAddrs) != 2 || c.ListenAddrs[1] != "/ip6/::/tcp/4001" {
		t.Errorf("Listen = %q", c.ListenAddrs)
	}
	if c.Caps != qtcd.CapPublic|qtcd.CapRelay|qtcd.CapMailbox || c.EnableDHT {
		t.Errorf("caps %v dht %v", c.Caps, c.EnableDHT)
	}
	if len(c.Inet.Gateways) != 2 || strings.Join(c.Inet.AllowCallsigns, ",") != "N1ADJ,W1AW" {
		t.Errorf("Inet = %+v", c.Inet)
	}
	if m := c.Inet.Modules['B']; m.Module != 'C' || m.Mode != qtcd.ModeNative || m.Reflector != "203.0.113.5:17000" {
		t.Errorf("Module B = %+v", m)
	}
	if fc.Admin != "" {
		t.Errorf("admin %q; absent means disabled", fc.Admin)
	}
}

func TestConfigNoInet(t *testing.T) {
	fc, err := loadConfig(writeConfig(t, "[General]\nCallsign=N1ADJ  P\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Inet != nil {
		t.Error("client face configured without [Inet] or [Module X]")
	}
	if len(fc.ListenAddrs) != 1 || !fc.EnableDHT {
		t.Errorf("defaults: listen %v dht %v", fc.ListenAddrs, fc.EnableDHT)
	}
}

// TestConfigMessagingOnly: an [Inet] section alone gives one messaging-only
// module, A; a module with neither Reflector nor Module is messaging-only;
// Mode defaults to qtc; with no HostsFile, names come from the M17
// Project's list.
func TestConfigMessagingOnly(t *testing.T) {
	fc, err := loadConfig(writeConfig(t, "[General]\nCallsign=N1ADJ  P\n[Inet]\nAllowCallsigns=W1AW\n"))
	if err != nil {
		t.Fatal(err)
	}
	in := fc.Inet
	if in == nil || len(in.Modules) != 1 || in.Modules['A'] != (qtcd.ModuleConfig{Mode: qtcd.ModeQTC}) {
		t.Fatalf("Inet = %+v", in)
	}
	if in.HostsURL != qtcd.DefaultHostsURL || in.HostsFile != "" || in.HostsRefresh != 0 {
		t.Errorf("hosts: file %q url %q refresh %v", in.HostsFile, in.HostsURL, in.HostsRefresh)
	}

	fc, err = loadConfig(writeConfig(t, `
[General]
Callsign = N1ADJ  P
[Inet]
HostsFile = /opt/m17/M17Hosts.txt
HostsRefresh = 6h
[Module A]
[Module B]
Reflector = M17-M17
Module = C
`))
	if err != nil {
		t.Fatal(err)
	}
	in = fc.Inet
	if in.Modules['A'] != (qtcd.ModuleConfig{Mode: qtcd.ModeQTC}) || in.Modules['B'] != (qtcd.ModuleConfig{Reflector: "M17-M17", Module: 'C', Mode: qtcd.ModeQTC}) {
		t.Errorf("Modules = %+v", in.Modules)
	}
	if in.HostsURL != "" || in.HostsRefresh != 6*time.Hour {
		t.Errorf("hosts: url %q refresh %v", in.HostsURL, in.HostsRefresh)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name, ini, want string
	}{
		{"typo", "[General]\nCallsign=N1ADJ  P\nDataDri=/x\n", "[General]: unknown setting datadri"},
		{"section", "[General]\nCallsign=N1ADJ  P\n[Netwrok]\n", "unknown section [netwrok]"},
		{"no section", "Callsign=N1ADJ  P\n", "every setting belongs in a section"},
		{"no callsign", "[General]\nDataDir=/x\n", "Callsign is required"},
		{"duration", "[General]\nCallsign=N1ADJ  P\n[Timers]\nSweepInterval=hourly\n", "[Timers] SweepInterval: \"hourly\" is not a duration"},
		{"bool", "[General]\nCallsign=N1ADJ  P\n[Network]\nDHT=maybe\n", "[Network] DHT: \"maybe\" is not true or false"},
		{"int", "[General]\nCallsign=N1ADJ  P\n[Network]\nK=two\n", "[Network] K: \"two\" is not a whole number"},
		{"reach", "[General]\nCallsign=N1ADJ  P\n[Delivery]\nReachWindow=an hour\n", "[Delivery] ReachWindow: \"an hour\" is not a duration"},
		{"cap", "[General]\nCallsign=N1ADJ  P\n[Network]\nCaps=public,mailbx\n", "unknown capability \"mailbx\""},
		{"cidr", "[General]\nCallsign=N1ADJ  P\n[Inet]\nGateways=10.0.0.0\n[Module A]\nReflector=M17-M17\nModule=C\nMode=qtc\n", "Gateways: \"10.0.0.0\" is not a CIDR"},
		{"hosts both", "[General]\nCallsign=N1ADJ  P\n[Inet]\nHostsFile=/x\nHostsURL=https://example.net/h.txt\n", "set HostsFile or HostsURL, not both"},
		{"hosts refresh", "[General]\nCallsign=N1ADJ  P\n[Inet]\nHostsRefresh=daily\n", "[Inet] HostsRefresh: \"daily\" is not a duration"},
		{"module name", "[General]\nCallsign=N1ADJ  P\n[Module AB]\n", "module sections are named [Module A]"},
		{"module letter", "[General]\nCallsign=N1ADJ  P\n[Module A]\nReflector=M17-M17\nModule=CC\nMode=qtc\n", "[Module A] Module must be one letter"},
		{"mode", "[General]\nCallsign=N1ADJ  P\n[Module A]\nReflector=M17-M17\nModule=C\nMode=proxy\n", "[Module A] Mode must be qtc or native"},
		{"reflector", "[General]\nCallsign=N1ADJ  P\n[Module A]\nModule=C\nMode=qtc\n", "[Module A] Module needs a Reflector"},
		{"native no reflector", "[General]\nCallsign=N1ADJ  P\n[Module A]\nMode=native\n", "[Module A] a native module needs a Reflector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.ini)
			_, err := loadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want it to contain %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), path+": ") {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}
