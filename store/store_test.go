package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jancona/qtc/envelope"
)

const t0 uint32 = 1789128000

var (
	n1adj  = mustAddr("N1ADJ")
	n1adjH = mustAddr("N1ADJ  H")
	w1aw   = mustAddr("W1AW")
	k1xyzR = mustAddr("K1XYZ  R")
)

func mustAddr(s string) envelope.Address {
	a, err := envelope.EncodeAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

func msg(t *testing.T, src, dst envelope.Address, ts uint32, ttl, nonce uint16, body string) *envelope.Envelope {
	t.Helper()
	e, err := envelope.BuildMsg(src, dst, ts, ttl, nonce, 0, body)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func rcpt(t *testing.T, id envelope.ID, st envelope.Status, ts uint32) *envelope.Envelope {
	t.Helper()
	e, err := envelope.BuildRcpt(k1xyzR, n1adj, id, st, ts, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// clock is a settable server clock.
type clock struct{ now uint32 }

func (c *clock) Now() uint32 { return c.now }

// pair connects a client to a fresh Serve loop over net.Pipe.
func pair(t *testing.T, srv *Server) (*Client, func()) {
	t.Helper()
	sc, cc := net.Pipe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(sc) }()
	cl := NewClient(cc, nil)
	var once sync.Once
	return cl, func() {
		once.Do(func() {
			cc.Close()
			sc.Close()
			select {
			case <-serveErr:
			case <-time.After(time.Second):
				t.Error("Serve did not return")
			}
			<-cl.Done()
		})
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestPutQueryDedup(t *testing.T) { eachStore(t, 0, testPutQueryDedup) }

func testPutQueryDedup(t *testing.T, mem testStore) {
	clk := &clock{t0}
	srv := NewServer(mem)
	srv.Now = clk.Now
	cl, stop := pair(t, srv)
	defer stop()

	m1 := msg(t, n1adjH, w1aw, t0, 1440, 1, "one")
	m2 := msg(t, n1adjH, w1aw, t0, 1440, 2, "two")
	for _, e := range []*envelope.Envelope{m1, m2, m1} { // m1 twice: second is a no-op
		if err := cl.Put(ctx(t), w1aw, e); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if mem.Len() != 2 {
		t.Errorf("stored %d, want 2", mem.Len())
	}

	// Receipts for one message coexist with it and with each other.
	r1 := rcpt(t, m1.ID(), envelope.StatusQueued, t0+1)
	r2 := rcpt(t, m1.ID(), envelope.StatusTransmitted, t0+2)
	clk.now = t0 + 10
	for _, e := range []*envelope.Envelope{r1, r2} {
		if err := cl.Put(ctx(t), n1adjH, e); err != nil { // device suffix: stored under N1ADJ
			t.Fatalf("Put rcpt: %v", err)
		}
	}
	if mem.Len() != 4 {
		t.Errorf("stored %d, want 4", mem.Len())
	}

	envs, _, more, err := cl.Query(ctx(t), w1aw, 0, 0, nil)
	if err != nil || more {
		t.Fatalf("Query: %v more=%v", err, more)
	}
	if got := bodies(envs); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Errorf("Query(W1AW) = %v", got)
	}
	envs, _, _, err = cl.Query(ctx(t), n1adj, 0, 0, nil)
	if err != nil || len(envs) != 2 {
		t.Fatalf("Query(N1ADJ) = %d envs, %v", len(envs), err)
	}
	// since is by received-at, inclusive.
	if envs, _, _, _ = cl.Query(ctx(t), n1adj, t0+10, 0, nil); len(envs) != 2 {
		t.Errorf("since=received-at: %d envs, want 2", len(envs))
	}
	if envs, _, _, _ = cl.Query(ctx(t), n1adj, t0+11, 0, nil); len(envs) != 0 {
		t.Errorf("since after received-at: %d envs, want 0", len(envs))
	}
	if envs, _, _, _ = cl.Query(ctx(t), mustAddr("AB1CD"), 0, 0, nil); len(envs) != 0 {
		t.Errorf("unknown callsign: %d envs", len(envs))
	}
}

func TestPutErrors(t *testing.T) { eachStore(t, 1, testPutErrors) }

func testPutErrors(t *testing.T, mem testStore) {
	clk := &clock{t0}
	srv := NewServer(mem)
	srv.Now = clk.Now
	cl, stop := pair(t, srv)
	defer stop()

	ok := msg(t, n1adj, w1aw, t0, 60, 1, "ok")
	if err := cl.Put(ctx(t), w1aw, ok); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		callsign envelope.Address
		env      *envelope.Envelope
		code     Code
	}{
		{"live only", n1adj, msg(t, n1adj, n1adj, t0, 0, 1, "x"), CodeRefused},
		{"expired", n1adj, msg(t, n1adj, n1adj, t0-7200, 60, 2, "x"), CodeExpired},
		{"quota", w1aw, msg(t, n1adj, w1aw, t0, 60, 3, "over"), CodeQuota},
		{"callsign is broadcast", envelope.Broadcast, msg(t, n1adj, w1aw, t0, 60, 4, "x"), CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cl.Put(ctx(t), tt.callsign, tt.env)
			var pe *PutError
			if !errors.As(err, &pe) || pe.Code != tt.code {
				t.Fatalf("Put err = %v, want code %s", err, tt.code)
			}
			if tt.code != CodeInvalid && pe.ID != tt.env.StoreID() {
				t.Errorf("PUT_ERR id %s, want %s", pe.ID, tt.env.StoreID())
			}
		})
	}
	if mem.Len() != 1 {
		t.Errorf("stored %d, want 1", mem.Len())
	}

	// Raw protocol: a garbage envelope gets PUT_ERR 4, unknown types are
	// ignored, and a bad JSON line ends the stream.
	sc, cc := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sc) }()
	rd := bufio.NewReader(cc)
	write := func(s string) {
		if _, err := cc.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"type":"FUTURE","x":1}`)
	write(`{"type":"PUT","callsign":"W1AW","env":"AAAA","extra":true}`)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m message
	if err := json.Unmarshal([]byte(line), &m); err != nil || m.Type != TypePutErr || m.Code != CodeInvalid {
		t.Errorf("got %s, want PUT_ERR 4", strings.TrimSpace(line))
	}
	write(`not json`)
	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned nil on bad JSON")
		}
	case <-time.After(time.Second):
		t.Error("Serve did not end on bad JSON")
	}
	cc.Close()
}

func TestQueryPagingAndTypes(t *testing.T) { eachStore(t, 0, testQueryPagingAndTypes) }

func testQueryPagingAndTypes(t *testing.T, mem testStore) {
	clk := &clock{t0}
	srv := NewServer(mem)
	srv.Now = clk.Now
	cl, stop := pair(t, srv)
	defer stop()

	// Five messages: two at t0, three at t0+1; plus one RCPT and one ROOM.
	var all []*envelope.Envelope
	for i := 0; i < 5; i++ {
		if i == 2 {
			clk.now = t0 + 1
		}
		e := msg(t, n1adj, w1aw, t0, 1440, uint16(i), string(rune('a'+i)))
		all = append(all, e)
		if err := cl.Put(ctx(t), w1aw, e); err != nil {
			t.Fatal(err)
		}
	}
	clk.now = t0 + 2
	if err := cl.Put(ctx(t), w1aw, rcpt(t, all[0].ID(), envelope.StatusQueued, t0)); err != nil {
		t.Fatal(err)
	}
	room, _ := envelope.BuildRoom(envelope.OpJoin, t0, []envelope.Address{mustRoomAddr("MAINE")}, "")
	if err := cl.Put(ctx(t), w1aw, room); err != nil {
		t.Fatal(err)
	}

	// limit 1 at t0 returns both t0 messages (same second) and points at t0+1.
	envs, next, more, err := cl.Query(ctx(t), w1aw, 0, 1, []envelope.PacketType{envelope.TypeMSG})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(envs); !reflect.DeepEqual(got, []string{"a", "b"}) || !more || next != t0 {
		t.Errorf("page 1 = %v more=%v next=%d", got, more, next)
	}
	// Querying again from next overlaps by design; page 2 from t0+1 gets the rest.
	envs, next, more, err = cl.Query(ctx(t), w1aw, t0+1, 2, []envelope.PacketType{envelope.TypeMSG})
	if err != nil {
		t.Fatal(err)
	}
	if got := bodies(envs); !reflect.DeepEqual(got, []string{"c", "d", "e"}) || more {
		t.Errorf("page 2 = %v more=%v next=%d", got, more, next)
	}

	// Default types are MSG and RCPT: 6, not 7.
	if envs, _, _, _ = cl.Query(ctx(t), w1aw, 0, 0, nil); len(envs) != 6 {
		t.Errorf("default types: %d envs, want 6", len(envs))
	}
	if envs, _, _, _ = cl.Query(ctx(t), w1aw, 0, 0, []envelope.PacketType{envelope.TypeROOM}); len(envs) != 1 {
		t.Errorf("ROOM only: %d envs, want 1", len(envs))
	}

	// QueryAll unions pages by StoreID.
	got, err := cl.QueryAll(ctx(t), w1aw, 0, []envelope.PacketType{envelope.TypeMSG, envelope.TypeRCPT, envelope.TypeROOM})
	if err != nil || len(got) != 7 {
		t.Errorf("QueryAll = %d envs, %v; want 7", len(got), err)
	}

	// The server clamps limit; the client sends what it was given.
	m := message{Type: TypeQuery}
	lim := MaxLimit + 1
	m.Limit = &lim
	if got := clampLimit(m); got != MaxLimit {
		t.Errorf("clamp = %d", got)
	}
}

func clampLimit(m message) int {
	limit := DefaultLimit
	if m.Limit != nil && *m.Limit > 0 {
		limit = min(*m.Limit, MaxLimit)
	}
	return limit
}

func TestWatchEvents(t *testing.T) {
	clk := &clock{t0}
	srv := NewServer(NewMemStore())
	srv.Now = clk.Now
	watcher, stopW := pair(t, srv)
	defer stopW()
	writer, stopP := pair(t, srv)
	defer stopP()

	if err := watcher.Watch(n1adjH, w1aw); err != nil { // device suffix normalizes to base
		t.Fatal(err)
	}
	waitWatchers(t, srv, n1adj, 1)

	m1 := msg(t, k1xyzR, n1adj, t0, 60, 1, "for n1adj")
	if err := writer.Put(ctx(t), n1adj, m1); err != nil {
		t.Fatal(err)
	}
	ev := recvEvent(t, watcher)
	if ev.Callsign != n1adj || ev.Env.ID() != m1.ID() {
		t.Errorf("event = %s %s", ev.Callsign, ev.Env)
	}

	// A duplicate PUT stores nothing and raises no event; an unwatched
	// callsign raises none either.
	if err := writer.Put(ctx(t), n1adj, m1); err != nil {
		t.Fatal(err)
	}
	if err := writer.Put(ctx(t), mustAddr("AB1CD"), msg(t, k1xyzR, n1adj, t0, 60, 2, "x")); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Unwatch(n1adj); err != nil {
		t.Fatal(err)
	}
	waitWatchers(t, srv, n1adj, 0)
	if err := writer.Put(ctx(t), n1adj, msg(t, k1xyzR, n1adj, t0, 60, 3, "after unwatch")); err != nil {
		t.Fatal(err)
	}
	// W1AW is still watched, so this event arrives, and nothing before it.
	m4 := msg(t, k1xyzR, w1aw, t0, 60, 4, "for w1aw")
	if err := writer.Put(ctx(t), w1aw, m4); err != nil {
		t.Fatal(err)
	}
	ev = recvEvent(t, watcher)
	if ev.Env.ID() != m4.ID() {
		t.Errorf("event = %s, want the W1AW message", ev.Env)
	}

	// Events interleave with a request on the same stream: the watcher
	// puts to W1AW and gets both its own EVENT and PUT_OK.
	m5 := msg(t, k1xyzR, w1aw, t0, 60, 5, "self")
	if err := watcher.Put(ctx(t), w1aw, m5); err != nil {
		t.Fatal(err)
	}
	if ev = recvEvent(t, watcher); ev.Env.ID() != m5.ID() {
		t.Errorf("self event = %s", ev.Env)
	}

	// Closing the stream drops its watches.
	stopW()
	waitWatchers(t, srv, w1aw, 0)
	if err := watcher.Put(ctx(t), w1aw, m5); err == nil {
		t.Error("Put after close succeeded")
	}
}

func TestClientCancelDiscardsLateReply(t *testing.T) {
	srv := NewServer(NewMemStore())
	srv.Now = func() uint32 { return t0 }
	// A server that never answers the first request: use a raw pipe and a
	// hand-driven peer.
	sc, cc := net.Pipe()
	cl := NewClient(cc, nil)
	defer func() { cc.Close(); sc.Close(); <-cl.Done() }()
	rd := bufio.NewReader(sc)

	c, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- cl.Put(c, w1aw, msg(t, n1adj, w1aw, t0, 60, 1, "x")) }()
	if _, err := rd.ReadString('\n'); err != nil { // the PUT arrives
		t.Fatal(err)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	// Late reply to the cancelled request, then the real reply to the next.
	go func() {
		sc.Write([]byte(`{"type":"PUT_ERR","id":"0000000000000000","code":1,"reason":"late"}` + "\n"))
		rd.ReadString('\n') // the second PUT
		sc.Write([]byte(`{"type":"PUT_OK","id":"0000000000000000"}` + "\n"))
	}()
	if err := cl.Put(ctx(t), w1aw, msg(t, n1adj, w1aw, t0, 60, 2, "y")); err != nil {
		t.Errorf("second Put = %v, want nil (late PUT_ERR must be discarded)", err)
	}
}

func TestPolicyExpiry(t *testing.T) {
	p := Policy{}
	tests := []struct {
		name string
		env  *envelope.Envelope
		want uint32
		code Code
	}{
		{"normal", msg(t, n1adj, w1aw, t0, 60, 1, ""), t0 + 3600, 0},
		{"unknown ts uses now", msg(t, n1adj, w1aw, 0, 60, 1, ""), t0 + 3600, 0},
		{"far future ts uses now", msg(t, n1adj, w1aw, t0+7200, 60, 1, ""), t0 + 3600, 0},
		{"within tolerance keeps ts", msg(t, n1adj, w1aw, t0+600, 60, 1, ""), t0 + 600 + 3600, 0},
		{"default ttl", msg(t, n1adj, w1aw, t0, envelope.TTLDefault, 1, ""), t0 + 7*86400, 0},
		{"live only", msg(t, n1adj, w1aw, t0, 0, 1, ""), 0, CodeRefused},
		{"expired", msg(t, n1adj, w1aw, t0-3600, 60, 1, ""), 0, CodeExpired},
		{"rcpt", rcpt(t, envelope.ID{}, envelope.StatusQueued, t0), t0 + 7*86400, 0},
	}
	room, _ := envelope.BuildRoom(envelope.OpJoin, t0, nil, "")
	tests = append(tests, struct {
		name string
		env  *envelope.Envelope
		want uint32
		code Code
	}{"room", room, t0 + 30*86400, 0})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp, err := p.Expiry(tt.env, t0)
			var pe *PutError
			if tt.code != 0 {
				if !errors.As(err, &pe) || pe.Code != tt.code {
					t.Fatalf("err = %v, want code %s", err, tt.code)
				}
				return
			}
			if err != nil || exp != tt.want {
				t.Errorf("= %d, %v; want %d", exp, err, tt.want)
			}
		})
	}
	capped := Policy{MaxTTL: 30}
	if exp, _ := capped.Expiry(msg(t, n1adj, w1aw, t0, 60, 1, ""), t0); exp != t0+1800 {
		t.Errorf("MaxTTL cap: %d", exp)
	}
}

func TestStoreExpire(t *testing.T) { eachStore(t, 0, testStoreExpire) }

func testStoreExpire(t *testing.T, mem testStore) {
	put := func(e *envelope.Envelope, recv, exp uint32) {
		if _, err := mem.Put(Record{Callsign: w1aw, Env: e, ReceivedAt: recv, Expiry: exp}); err != nil {
			t.Fatal(err)
		}
	}
	put(msg(t, n1adj, w1aw, t0, 60, 1, "a"), t0+5, t0+100)
	put(msg(t, n1adj, w1aw, t0, 60, 2, "b"), t0+1, t0+200) // out-of-order receipt sorts first
	recs, _, _, _ := mem.Query(w1aw, 0, 10, nil)
	if got := bodiesRec(recs); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Errorf("order = %v", got)
	}
	if n := mem.Expire(t0 + 99); n != 0 {
		t.Errorf("expired %d early", n)
	}
	if n := mem.Expire(t0 + 100); n != 1 || mem.Len() != 1 {
		t.Errorf("Expire = %d, len %d", n, mem.Len())
	}
	if n := mem.Expire(t0 + 300); n != 1 || mem.Len() != 0 {
		t.Errorf("Expire = %d, len %d", n, mem.Len())
	}
}

func TestWireResultShape(t *testing.T) {
	b, err := json.Marshal(message{Type: TypeResult})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"RESULT","envs":[],"next":null}` {
		t.Errorf("empty RESULT = %s", b)
	}
	b, _ = json.Marshal(message{Type: TypePutOK, ID: "00"})
	if strings.Contains(string(b), "envs") || strings.Contains(string(b), "next") {
		t.Errorf("PUT_OK carries RESULT fields: %s", b)
	}
	if _, err := parseID("zz"); err == nil {
		t.Error("parseID accepted junk")
	}
}

func mustRoomAddr(name string) envelope.Address {
	a, err := envelope.RoomAddress(name)
	if err != nil {
		panic(err)
	}
	return a
}

func bodies(envs []*envelope.Envelope) []string {
	out := []string{}
	for _, e := range envs {
		if m, ok := e.Msg(); ok {
			out = append(out, m.Body())
		}
	}
	return out
}

func bodiesRec(recs []Record) []string {
	envs := make([]*envelope.Envelope, len(recs))
	for i, r := range recs {
		envs[i] = r.Env
	}
	return bodies(envs)
}

func recvEvent(t *testing.T, c *Client) Event {
	t.Helper()
	select {
	case ev, ok := <-c.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func waitWatchers(t *testing.T, s *Server, a envelope.Address, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.Watchers(a) != n {
		if time.Now().After(deadline) {
			t.Fatalf("watchers(%s) = %d, want %d", a, s.Watchers(a), n)
		}
		time.Sleep(time.Millisecond)
	}
}
