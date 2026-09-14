package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/jancona/pigeon/envelope"
)

// Event is an EVENT message: a newly stored envelope for a watched callsign.
type Event struct {
	Callsign envelope.Address
	Env      *envelope.Envelope
}

// ErrClosed is returned once the client's stream has ended.
var ErrClosed = errors.New("store: stream closed")

// Client speaks the storage protocol over one stream to an inbox node. It
// keeps one request in flight at a time, since the protocol has no request
// IDs and replies come back in order. Events arrive on Events regardless of
// requests; the caller must drain them or requests will stall.
type Client struct {
	w   io.Writer
	wmu sync.Mutex

	reqMu   sync.Mutex // one request at a time
	replies chan message
	stale   int32 // replies to discard after a cancelled request; guarded by reqMu
	events  chan Event
	done    chan struct{}
	err     error
	log     *slog.Logger
}

// NewClient starts reading from rw and returns a Client. The caller owns
// rw and closes it to end the session; Done is closed when the reader
// stops, and Err reports why.
func NewClient(rw io.ReadWriter, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	c := &Client{w: rw, replies: make(chan message), events: make(chan Event, 64), done: make(chan struct{}), log: log}
	go c.read(rw)
	return c
}

// Events delivers EVENT messages. It is closed when the stream ends.
func (c *Client) Events() <-chan Event { return c.events }

// Done is closed when the stream ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the stream ended: nil for a clean EOF, or the error.
// It is only meaningful after Done is closed.
func (c *Client) Err() error { return c.err }

func (c *Client) read(r io.Reader) {
	defer close(c.done)
	defer close(c.events)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxMessage)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			c.err = fmt.Errorf("store: bad message: %w", err)
			return
		}
		switch m.Type {
		case TypeEvent:
			a, err := envelope.ParseAddress(m.Callsign)
			if err != nil {
				c.log.Warn("store: EVENT with bad callsign", "callsign", m.Callsign, "err", err)
				continue
			}
			e, err := decodeEnv(m.Env)
			if err != nil {
				c.log.Warn("store: EVENT with bad envelope", "err", err)
				continue
			}
			c.events <- Event{Callsign: a, Env: e}
		case TypePutOK, TypePutErr, TypeResult:
			select {
			case c.replies <- m:
			case <-c.done:
				return
			}
		default:
			c.log.Warn("store: ignoring unknown message type", "type", m.Type)
		}
	}
	if err := sc.Err(); err != nil {
		c.err = fmt.Errorf("store: read: %w", err)
	}
}

func (c *Client) send(m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", m.Type, err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("store: write %s: %w", m.Type, err)
	}
	return nil
}

// request sends m and waits for the next reply. A reply that arrives after
// ctx is cancelled is discarded before the next request.
func (c *Client) request(ctx context.Context, m message) (message, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	for c.stale > 0 {
		select {
		case <-c.replies:
			c.stale--
		case <-c.done:
			return message{}, ErrClosed
		case <-ctx.Done():
			return message{}, ctx.Err()
		}
	}
	if err := c.send(m); err != nil {
		return message{}, err
	}
	select {
	case r := <-c.replies:
		return r, nil
	case <-c.done:
		return message{}, ErrClosed
	case <-ctx.Done():
		c.stale++
		return message{}, ctx.Err()
	}
}

// Put stores e under callsign (normalized to its base). A refusal is a
// *PutError.
func (c *Client) Put(ctx context.Context, callsign envelope.Address, e *envelope.Envelope) error {
	r, err := c.request(ctx, message{Type: TypePut, Callsign: callsign.Base().String(), Env: encodeEnv(e)})
	if err != nil {
		return err
	}
	switch r.Type {
	case TypePutOK:
		return nil
	case TypePutErr:
		id, _ := parseID(r.ID)
		return &PutError{ID: id, Code: r.Code, Reason: r.Reason}
	}
	return fmt.Errorf("store: unexpected %s reply to PUT", r.Type)
}

// Query fetches envelopes stored for callsign since the given received-at
// time. types empty means the server default (MSG and RCPT). more is true
// when another page exists starting at next.
func (c *Client) Query(ctx context.Context, callsign envelope.Address, since uint32, limit int, types []envelope.PacketType) (envs []*envelope.Envelope, next uint32, more bool, err error) {
	m := message{Type: TypeQuery, Callsign: callsign.Base().String(), Since: &since}
	if limit > 0 {
		m.Limit = &limit
	}
	for _, t := range types {
		m.Types = append(m.Types, int(t))
	}
	r, err := c.request(ctx, m)
	if err != nil {
		return nil, 0, false, err
	}
	if r.Type != TypeResult {
		return nil, 0, false, fmt.Errorf("store: unexpected %s reply to QUERY", r.Type)
	}
	for _, s := range r.Envs {
		e, err := decodeEnv(s)
		if err != nil {
			c.log.Warn("store: RESULT with bad envelope", "err", err)
			continue
		}
		envs = append(envs, e)
	}
	if r.Next != nil {
		return envs, *r.Next, true, nil
	}
	return envs, 0, false, nil
}

// QueryAll pages through Query until no more remain, deduplicating by
// StoreID since pages may overlap.
func (c *Client) QueryAll(ctx context.Context, callsign envelope.Address, since uint32, types []envelope.PacketType) ([]*envelope.Envelope, error) {
	seen := map[envelope.ID]bool{}
	var out []*envelope.Envelope
	for {
		envs, next, more, err := c.Query(ctx, callsign, since, MaxLimit, types)
		if err != nil {
			return out, err
		}
		for _, e := range envs {
			if id := e.StoreID(); !seen[id] {
				seen[id] = true
				out = append(out, e)
			}
		}
		if !more || len(envs) == 0 {
			return out, nil
		}
		since = next
	}
}

// Watch asks for EVENTs for the callsigns (normalized to base) on this
// stream. There is no reply.
func (c *Client) Watch(callsigns ...envelope.Address) error {
	return c.send(message{Type: TypeWatch, Callsigns: addressTexts(callsigns)})
}

// Unwatch stops EVENTs for the callsigns.
func (c *Client) Unwatch(callsigns ...envelope.Address) error {
	return c.send(message{Type: TypeUnwatch, Callsigns: addressTexts(callsigns)})
}

func addressTexts(as []envelope.Address) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Base().String()
	}
	return out
}

func parseID(s string) (envelope.ID, error) {
	var id envelope.ID
	if len(s) != 2*envelope.IDLen {
		return id, fmt.Errorf("store: bad id %q", s)
	}
	for i := 0; i < envelope.IDLen; i++ {
		var b byte
		if _, err := fmt.Sscanf(s[2*i:2*i+2], "%02x", &b); err != nil {
			return id, fmt.Errorf("store: bad id %q", s)
		}
		id[i] = b
	}
	return id, nil
}
