package roost

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jancona/pigeon/envelope"
)

// Admin is a small HTTP interface on a local address for driving a roost by
// hand during the spike, standing in for the M17_inet face. Endpoints:
//
//	GET  /status                      node ID, peers, homed callsigns, rooms, presence
//	POST /send?from=&to=&body=&rcpt=1 build and Send a MSG (to may be a room or #ROOM)
//	POST /room?device=&op=join|leave|list&rooms=A,B
//	POST /heard?device=               mark a device heard via local client
//
// It must only listen on a loopback address.
type Admin struct {
	r   *Roost
	srv *http.Server
}

// ServeAdmin starts the admin listener on addr and returns it.
func (r *Roost) ServeAdmin(addr string) (*Admin, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("roost: admin addr %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("roost: admin addr %q is not a loopback address", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("roost: admin listen: %w", err)
	}
	a := &Admin{r: r}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", a.status)
	mux.HandleFunc("POST /send", a.send)
	mux.HandleFunc("POST /room", a.room)
	mux.HandleFunc("POST /heard", a.heard)
	a.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	r.go_(func() {
		if err := a.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			r.log.Warn("admin server", "err", err)
		}
	})
	r.go_(func() {
		<-r.ctx.Done()
		a.srv.Close()
	})
	r.log.Info("admin listening", "addr", ln.Addr())
	return a, nil
}

func (a *Admin) status(w http.ResponseWriter, req *http.Request) {
	r := a.r
	type nodeOut struct {
		Peer     string   `json:"peer"`
		Callsign string   `json:"callsign"`
		Caps     int      `json:"caps"`
		LastSeen uint32   `json:"last_seen"`
		Devices  []string `json:"devices"`
	}
	var nodes []nodeOut
	for _, id := range r.presence.Nodes() {
		n, _ := r.presence.Node(id)
		no := nodeOut{Peer: id.String(), Callsign: n.Callsign.String(), Caps: int(n.Caps), LastSeen: n.LastSeen}
		for d, info := range n.Devices {
			no.Devices = append(no.Devices, fmt.Sprintf("%s via %d last %d", d, info.Via, info.Last))
		}
		nodes = append(nodes, no)
	}
	var homed []string
	for _, a := range r.Homed() {
		rooms, _ := r.subs.Rooms(a)
		homed = append(homed, fmt.Sprintf("%s rooms=%v", a, rooms))
	}
	var peers []string
	for _, p := range r.host.Network().Peers() {
		var addrs []string
		for _, c := range r.host.Network().ConnsToPeer(p) {
			addrs = append(addrs, c.RemoteMultiaddr().String())
		}
		peers = append(peers, p.String()+" "+strings.Join(addrs, ","))
	}
	out := map[string]any{
		"id":       r.host.ID().String(),
		"callsign": r.cfg.Callsign,
		"addrs":    addrStrings(r),
		"peers":    peers,
		"homed":    homed,
		"rooms":    r.subs.ActiveRooms(),
		"presence": nodes,
	}
	if r.mem != nil {
		out["stored"] = r.mem.Len()
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}

func addrStrings(r *Roost) []string {
	var out []string
	for _, a := range r.host.Addrs() {
		out = append(out, a.String())
	}
	return out
}

func (a *Admin) send(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	from, err := envelope.EncodeAddress(q.Get("from"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	to, err := envelope.ParseAddress(q.Get("to"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var flags byte
	if q.Get("rcpt") == "1" {
		flags |= envelope.FlagRcptReq
	}
	ttl := uint16(60)
	if s := q.Get("ttl"); s != "" {
		v, err := strconv.ParseUint(s, 10, 16)
		if err != nil {
			http.Error(w, "bad ttl", http.StatusBadRequest)
			return
		}
		ttl = uint16(v)
	}
	nonce, err := envelope.NewNonce()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e, err := envelope.BuildMsg(from, to, unixNow(), ttl, nonce, flags, q.Get("body"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.r.Heard(from, ViaLocal, unixNow()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.r.Send(e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, "sent %s\n", e)
}

func (a *Admin) room(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	device, err := envelope.EncodeAddress(q.Get("device"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var op envelope.RoomOp
	switch strings.ToLower(q.Get("op")) {
	case "join":
		op = envelope.OpJoin
	case "leave":
		op = envelope.OpLeave
	case "list":
		op = envelope.OpList
	default:
		http.Error(w, "op must be join, leave, or list", http.StatusBadRequest)
		return
	}
	var rooms []envelope.Address
	for _, name := range strings.Split(q.Get("rooms"), ",") {
		if name == "" {
			continue
		}
		ra, err := envelope.RoomAddress(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rooms = append(rooms, ra)
	}
	pkt, err := envelope.BuildRoom(op, unixNow(), rooms, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.r.Heard(device, ViaLocal, unixNow()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply, err := a.r.HandleRoom(device, pkt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, "%s\n", reply)
}

func (a *Admin) heard(w http.ResponseWriter, req *http.Request) {
	device, err := envelope.EncodeAddress(req.URL.Query().Get("device"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.r.Heard(device, ViaLocal, unixNow()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, "heard %s\n", device)
}
