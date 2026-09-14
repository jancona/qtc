package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jancona/pigeon/envelope"
)

// Server serves the storage protocol against a Store. One Server handles
// any number of streams; call Serve once per stream.
type Server struct {
	Store  Store
	Policy Policy
	// Now returns the current Unix time; nil uses the wall clock.
	Now func() uint32
	// Log receives protocol-level warnings; nil uses slog.Default.
	Log *slog.Logger

	mu       sync.Mutex
	watchers map[envelope.Address]map[*conn]struct{}
}

// NewServer returns a Server over st with default policy.
func NewServer(st Store) *Server {
	return &Server{Store: st, watchers: map[envelope.Address]map[*conn]struct{}{}}
}

func (s *Server) now() uint32 {
	if s.Now != nil {
		return s.Now()
	}
	return uint32(time.Now().Unix())
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// conn is one served stream.
type conn struct {
	w  io.Writer
	mu sync.Mutex // serializes writes: replies and EVENTs interleave
}

func (c *conn) send(m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", m.Type, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Serve handles one stream until the reader returns EOF or an error, or a
// write fails. It returns nil on a clean EOF. Cancel it by closing rw.
func (s *Server) Serve(rw io.ReadWriter) error {
	c := &conn{w: rw}
	defer s.unwatchAll(c)
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 64*1024), MaxMessage)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("store: bad message: %w", err)
		}
		var err error
		switch m.Type {
		case TypePut:
			err = s.put(c, m)
		case TypeQuery:
			err = s.query(c, m)
		case TypeWatch:
			s.watch(c, m.Callsigns)
		case TypeUnwatch:
			s.unwatch(c, m.Callsigns)
		default:
			s.log().Warn("store: ignoring unknown message type", "type", m.Type)
		}
		if err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("store: message exceeds %d bytes", MaxMessage)
		}
		return fmt.Errorf("store: read: %w", err)
	}
	return nil
}

func (s *Server) put(c *conn, m message) error {
	fail := func(id envelope.ID, code Code, reason string) error {
		return c.send(message{Type: TypePutErr, ID: id.String(), Code: code, Reason: reason})
	}
	callsign, err := parseCallsign(m.Callsign)
	if err != nil {
		return fail(envelope.ID{}, CodeInvalid, err.Error())
	}
	e, err := decodeEnv(m.Env)
	if err != nil {
		return fail(envelope.ID{}, CodeInvalid, err.Error())
	}
	id := e.StoreID()
	now := s.now()
	exp, err := s.Policy.Expiry(e, now)
	if err != nil {
		var pe *PutError
		if errors.As(err, &pe) {
			return fail(id, pe.Code, pe.Reason)
		}
		return fail(id, CodeRefused, err.Error())
	}
	rec := Record{Callsign: callsign, Env: e, ReceivedAt: now, Expiry: exp}
	stored, err := s.Store.Put(rec)
	switch {
	case errors.Is(err, ErrQuota):
		return fail(id, CodeQuota, err.Error())
	case err != nil:
		return fail(id, CodeRefused, err.Error())
	}
	if err := c.send(message{Type: TypePutOK, ID: id.String()}); err != nil {
		return err
	}
	if stored {
		s.notify(callsign, e)
	}
	return nil
}

func (s *Server) query(c *conn, m message) error {
	callsign, err := parseCallsign(m.Callsign)
	if err != nil {
		s.log().Warn("store: QUERY with bad callsign", "callsign", m.Callsign, "err", err)
		return c.send(message{Type: TypeResult})
	}
	var since uint32
	if m.Since != nil {
		since = *m.Since
	}
	limit := DefaultLimit
	if m.Limit != nil && *m.Limit > 0 {
		limit = min(*m.Limit, MaxLimit)
	}
	types := []envelope.PacketType{envelope.TypeMSG, envelope.TypeRCPT}
	if len(m.Types) > 0 {
		types = types[:0]
		for _, t := range m.Types {
			types = append(types, envelope.PacketType(t))
		}
	}
	recs, next, more, err := s.Store.Query(callsign, since, limit, types)
	if err != nil {
		return fmt.Errorf("store: query: %w", err)
	}
	res := message{Type: TypeResult, Envs: make([]string, 0, len(recs))}
	for _, r := range recs {
		res.Envs = append(res.Envs, encodeEnv(r.Env))
	}
	if more {
		res.Next = &next
	}
	return c.send(res)
}

func (s *Server) watch(c *conn, callsigns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, text := range callsigns {
		a, err := parseCallsign(text)
		if err != nil {
			s.log().Warn("store: WATCH with bad callsign", "callsign", text, "err", err)
			continue
		}
		set := s.watchers[a]
		if set == nil {
			set = map[*conn]struct{}{}
			s.watchers[a] = set
		}
		set[c] = struct{}{}
	}
}

func (s *Server) unwatch(c *conn, callsigns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, text := range callsigns {
		a, err := parseCallsign(text)
		if err != nil {
			continue
		}
		s.dropWatcher(a, c)
	}
}

func (s *Server) unwatchAll(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for a := range s.watchers {
		s.dropWatcher(a, c)
	}
}

// dropWatcher: caller holds s.mu.
func (s *Server) dropWatcher(a envelope.Address, c *conn) {
	if set := s.watchers[a]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(s.watchers, a)
		}
	}
}

// notify sends an EVENT to every stream watching callsign. A failed write
// is left for that stream's Serve loop to discover.
func (s *Server) notify(callsign envelope.Address, e *envelope.Envelope) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.watchers[callsign]))
	for c := range s.watchers[callsign] {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	ev := message{Type: TypeEvent, Callsign: callsign.String(), Env: encodeEnv(e)}
	for _, c := range conns {
		if err := c.send(ev); err != nil {
			s.log().Warn("store: EVENT write failed", "callsign", callsign, "err", err)
		}
	}
}

// Watchers reports how many streams are watching callsign.
func (s *Server) Watchers(callsign envelope.Address) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers[callsign.Base()])
}
