package qtcd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func startFace(t *testing.T, core *stubCore, cfg InetConfig) *inetFace {
	t.Helper()
	face, err := newInetFace(core, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { face.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return face
}

func (f *inetFace) upstreamOf(module byte) *net.UDPAddr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upstream[module]
}

// TestMessagingOnlyModule: a module with no reflector links clients and
// takes SMS into QTC; there is nothing upstream, so voice goes nowhere.
func TestMessagingOnlyModule(t *testing.T) {
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face := startFace(t, core, InetConfig{Listen: "127.0.0.1:0", Modules: map[byte]ModuleConfig{'A': {Mode: ModeQTC}}})
	station := face.Addr()
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")

	gw := newUDPPeer(t)
	gw.sendTo(t, station, connDatagram(mustAddr(t, "N1ADJ  G"), 'A'))
	gw.expect(t, magicACKN)
	gw.sendTo(t, station, streamDatagram(w1aw, ht))
	gw.sendTo(t, station, smsDatagram(w1aw, ht, "hello"))
	eventually(t, "SMS ingested", 2*time.Second, func() bool { return core.sentCount() == 1 })
	core.mu.Lock()
	if len(core.heard) != 1 || core.heard[0] != ht {
		t.Errorf("heard %v; want %s once (stream, then SMS within the rate limit)", core.heard, ht)
	}
	core.mu.Unlock()
	gw.expect(t, magicPING) // the node keeps the link alive itself
	gw.sendTo(t, station, controlDatagram(magicDISC, mustAddr(t, "N1ADJ  G")))
	eventually(t, "session closed", 2*time.Second, func() bool {
		face.mu.Lock()
		defer face.mu.Unlock()
		return len(face.sessions) == 0
	})

	if _, err := newInetFace(core, InetConfig{Listen: "127.0.0.1:0", Modules: map[byte]ModuleConfig{'A': {Mode: ModeNative}}}); err == nil {
		t.Error("accepted a native module with no reflector")
	}
}

// TestHostsDownload: with a HostsURL, a named module refuses links until
// the list downloads, then links; the download is cached, and a later start
// resolves from the cache before (or without) downloading.
func TestHostsDownload(t *testing.T) {
	old := hostsRetry
	hostsRetry = 100 * time.Millisecond
	t.Cleanup(func() { hostsRetry = old })

	upstream := newUDPPeer(t)
	list := fmt.Sprintf("# test\nM17-UP 127.0.0.1 %d\n", upstream.addr().Port)
	var up atomic.Bool
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, list)
	}))
	t.Cleanup(srv.Close)
	cache := filepath.Join(t.TempDir(), "M17Hosts.txt")
	cfg := InetConfig{
		Listen: "127.0.0.1:0", HostsURL: srv.URL, HostsCache: cache, UserAgent: "qtcd/test",
		Modules: map[byte]ModuleConfig{'A': {Reflector: "M17-UP", Module: 'C', Mode: ModeQTC}},
	}
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face := startFace(t, core, cfg)
	gw := newUDPPeer(t)
	gwCall := mustAddr(t, "N1ADJ  G")
	gw.sendTo(t, face.Addr(), connDatagram(gwCall, 'A'))
	// Not ready yet: no answer at all, since a gateway does not retry
	// after NACK.
	gw.expectNone(t, magicNACK, 300*time.Millisecond)
	gw.expectNone(t, magicACKN, 10*time.Millisecond)

	up.Store(true)
	eventually(t, "module resolved", 3*time.Second, func() bool { return face.upstreamOf('A') != nil })
	gw.sendTo(t, face.Addr(), connDatagram(gwCall, 'A'))
	gw.expect(t, magicACKN)
	upstream.expect(t, magicCONN)
	if got := ua.Load(); got != "qtcd/test" {
		t.Errorf("User-Agent %v", got)
	}
	if b, err := os.ReadFile(cache); err != nil || string(b) != list {
		t.Errorf("cache = %q, %v", b, err)
	}

	srv.Close()
	f2, err := newInetFace(core, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.conn.Close()
	if got := f2.upstreamOf('A'); got == nil || got.Port != upstream.addr().Port {
		t.Errorf("from cache: upstream %v", got)
	}
}

// TestHostsFileReread: a HostsFile is read again every HostsRefresh, so a
// reflector added to it (a dashboard update, say) becomes usable.
func TestHostsFileReread(t *testing.T) {
	upstream := newUDPPeer(t)
	path := filepath.Join(t.TempDir(), "M17Hosts.txt")
	if err := writeFile(path, "M17-OTHER 192.0.2.1 17000\n"); err != nil {
		t.Fatal(err)
	}
	core := &stubCore{node: mustAddr(t, "N1ADJ  Z"), local: mustRoom(t, "N1ADJ")}
	face := startFace(t, core, InetConfig{
		Listen: "127.0.0.1:0", HostsFile: path, HostsRefresh: 100 * time.Millisecond,
		Modules: map[byte]ModuleConfig{'A': {Reflector: "M17-UP", Module: 'C', Mode: ModeQTC}},
	})
	if face.upstreamOf('A') != nil {
		t.Fatal("resolved a name missing from the hosts file")
	}
	if err := writeFile(path, fmt.Sprintf("M17-UP 127.0.0.1 %d\n", upstream.addr().Port)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "module resolved", 3*time.Second, func() bool { return face.upstreamOf('A') != nil })
}

func TestParseHostsSkipsBadLines(t *testing.T) {
	hosts, err := parseHosts([]byte("M17-A 192.0.2.1 17000\nM17-B 192.0.2.2\nM17-C 192.0.2.3 port\n"), "test")
	if err != nil || len(hosts) != 1 || hosts["M17-A"].Addr != "192.0.2.1:17000" {
		t.Errorf("hosts %v, err %v", hosts, err)
	}
	if _, err := parseHosts([]byte("# nothing\n"), "test"); err == nil {
		t.Error("accepted a list with no reflectors")
	}
}
