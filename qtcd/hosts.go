package qtcd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultHostsURL is the M17 Project's published reflector list.
const DefaultHostsURL = "https://m17-project.github.io/hostfiles/M17Hosts.txt"

// reflectorHost is one line of an M17 hosts file.
type reflectorHost struct {
	Name string
	Addr string // host:port
}

// loadHostsFile parses an M17Hosts.txt-style file.
func loadHostsFile(path string) (map[string]reflectorHost, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("qtcd: hosts file: %w", err)
	}
	return parseHosts(b, path)
}

// parseHosts parses "NAME HOST PORT" per line, space or tab separated, '#'
// comments. Names are upper-cased. A malformed line is skipped, so one bad
// entry in a published list does not lose the rest; a list with no usable
// entry at all is an error.
func parseHosts(b []byte, source string) (map[string]reflectorHost, error) {
	hosts := map[string]reflectorHost{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	line, bad := 0, ""
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < 3 {
			bad = fmt.Sprintf("line %d: need NAME HOST PORT", line)
			continue
		}
		port, err := strconv.ParseUint(fields[2], 10, 16)
		if err != nil {
			bad = fmt.Sprintf("line %d: bad port %q", line, fields[2])
			continue
		}
		name := strings.ToUpper(fields[0])
		hosts[name] = reflectorHost{Name: name, Addr: net.JoinHostPort(fields[1], strconv.FormatUint(port, 10))}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("qtcd: hosts file %s: %w", source, err)
	}
	if len(hosts) == 0 {
		if bad != "" {
			return nil, fmt.Errorf("qtcd: hosts file %s %s", source, bad)
		}
		return nil, fmt.Errorf("qtcd: hosts file %s lists no reflectors", source)
	}
	return hosts, nil
}

// maxHostsSize bounds a downloaded hosts file; the published one is a few KB.
const maxHostsSize = 1 << 20

// fetchHosts downloads a hosts file and, when cache is set, saves it there
// for the next start.
func fetchHosts(ctx context.Context, url, cache, userAgent string) (map[string]reflectorHost, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("qtcd: hosts URL: %w", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qtcd: hosts URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qtcd: hosts URL %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHostsSize))
	if err != nil {
		return nil, fmt.Errorf("qtcd: hosts URL %s: %w", url, err)
	}
	hosts, err := parseHosts(b, url)
	if err != nil {
		return nil, err
	}
	if cache != "" {
		if err := writeFileAtomic(cache, b); err != nil {
			return hosts, fmt.Errorf("qtcd: hosts cache: %w", err)
		}
	}
	return hosts, nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// hasUpstreams reports whether any module has an upstream reflector.
func (f *inetFace) hasUpstreams() bool {
	for _, m := range f.cfg.Modules {
		if m.Reflector != "" {
			return true
		}
	}
	return false
}

// needsHosts reports whether any module names its reflector rather than
// giving a host:port.
func (f *inetFace) needsHosts() bool {
	for _, m := range f.cfg.Modules {
		if m.Reflector != "" && !strings.Contains(m.Reflector, ":") {
			return true
		}
	}
	return false
}

// resolve sets each module's upstream address from hosts, replacing the
// previous set, and reports whether every module resolved. A module that
// does not resolve refuses links; sessions already linked keep their
// upstream until they relink. quiet skips the warning for a name not in
// hosts, for a first start still waiting on its download.
func (f *inetFace) resolve(hosts map[string]reflectorHost, quiet bool) bool {
	up := map[byte]*net.UDPAddr{}
	all := true
	for letter, m := range f.cfg.Modules {
		if m.Reflector == "" {
			continue
		}
		addr := m.Reflector
		if !strings.Contains(addr, ":") {
			h, ok := hosts[strings.ToUpper(addr)]
			if !ok {
				all = false
				if quiet {
					continue
				}
				f.log.Warn("reflector not found in hosts file; module refuses links until it is",
					"module", string(letter), "reflector", m.Reflector, "hosts", len(hosts))
				continue
			}
			addr = h.Addr
		}
		ua, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			all = false
			f.log.Warn("cannot resolve reflector; module refuses links until it resolves",
				"module", string(letter), "reflector", m.Reflector, "err", err)
			continue
		}
		up[letter] = ua
	}
	f.mu.Lock()
	old := f.upstream
	f.upstream = up
	f.mu.Unlock()
	for letter, ua := range up {
		if o := old[letter]; o == nil || o.String() != ua.String() {
			f.log.Info("module upstream", "module", string(letter), "reflector", f.cfg.Modules[letter].Reflector, "upstream", ua)
		}
	}
	return all
}

// refreshHosts resolves module upstreams again every HostsRefresh:
// re-reading HostsFile, or downloading HostsURL (first right away, since
// start used only the cache). After a failure it tries again sooner, and
// keeps the addresses it has.
func (f *inetFace) refreshHosts() {
	defer f.wg.Done()
	download := f.cfg.HostsFile == "" && f.cfg.HostsURL != "" && f.needsHosts()
	retry := min(hostsRetry, f.cfg.HostsRefresh)
	wait := f.cfg.HostsRefresh
	if download {
		wait = 0
	}
	for {
		t := time.NewTimer(wait)
		select {
		case <-f.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		var hosts map[string]reflectorHost
		var err error
		switch {
		case !f.needsHosts():
		case download:
			hosts, err = fetchHosts(f.ctx, f.cfg.HostsURL, f.cfg.HostsCache, f.cfg.UserAgent)
			if hosts != nil {
				f.log.Info("hosts file downloaded", "url", f.cfg.HostsURL, "reflectors", len(hosts))
			}
		default:
			hosts, err = loadHostsFile(f.cfg.HostsFile)
		}
		if err != nil {
			if f.ctx.Err() != nil {
				return
			}
			f.log.Warn("hosts refresh", "err", err)
		}
		wait = f.cfg.HostsRefresh
		if f.needsHosts() && hosts == nil {
			wait = retry // keep the current addresses
			continue
		}
		if !f.resolve(hosts, false) {
			wait = retry
		}
	}
}
