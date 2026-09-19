package roost

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// reflectorHost is one line of an M17 hosts file.
type reflectorHost struct {
	Name string
	Addr string // host:port
}

// loadHostsFile parses an M17Hosts.txt-style file: "NAME HOST PORT" per
// line, space or tab separated, '#' comments. Names are upper-cased.
func loadHostsFile(path string) (map[string]reflectorHost, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("roost: hosts file: %w", err)
	}
	defer f.Close()
	hosts := map[string]reflectorHost{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < 3 {
			return nil, fmt.Errorf("roost: hosts file %s line %d: need NAME HOST PORT", path, line)
		}
		port, err := strconv.ParseUint(fields[2], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("roost: hosts file %s line %d: bad port %q", path, line, fields[2])
		}
		name := strings.ToUpper(fields[0])
		hosts[name] = reflectorHost{Name: name, Addr: net.JoinHostPort(fields[1], strconv.FormatUint(port, 10))}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("roost: hosts file %s: %w", path, err)
	}
	return hosts, nil
}
