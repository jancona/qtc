// Package station is the QTC node daemon as a library. This file holds room
// subscription state (rooms spec §4–§6); the M17_inet and libp2p faces live
// in their own files. Everything here is standard library plus envelope.
package qtcd

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jancona/qtc/envelope"
)

// DefaultSubscriptionExpiry is how long a subscription lives after the
// callsign was last heard or last joined (rooms spec §4.5): 30 days.
const DefaultSubscriptionExpiry uint32 = 30 * 24 * 3600

// Errors from the subscription state.
var (
	ErrNotCallsign  = errors.New("qtcd: address is not a callsign")
	ErrNotRoomOp    = errors.New("qtcd: not a JOIN or LEAVE")
	ErrNoTimestamp  = errors.New("qtcd: ROOM envelope has timestamp 0; substitute the receipt time first")
	ErrNotRequest   = errors.New("qtcd: ROOM packet is a reply, not a request")
	ErrNotCommand   = errors.New("qtcd: text is not a room command")
	ErrBadRoomName  = errors.New("qtcd: invalid room name")
	ErrUnknownCmd   = errors.New("qtcd: unknown command")
	ErrNoLocalRoom  = errors.New("qtcd: node callsign does not yield a valid local room name")
	ErrNotValidRoom = errors.New("qtcd: not a valid room address")
)

// SubscriptionsConfig configures Subscriptions.
type SubscriptionsConfig struct {
	// NodeCallsign is this node's callsign, e.g. "K1XYZ  R". Its base
	// callsign names the local room (rooms spec §3.4). Empty disables the
	// local room and auto-subscription.
	NodeCallsign string
	// Expiry in seconds after last-refreshed; 0 means DefaultSubscriptionExpiry.
	Expiry uint32
	// Carries reports whether this node carries a room. nil carries all
	// valid rooms. The local room is always carried.
	Carries func(envelope.Address) bool
}

// Subscriptions is a node's view of which base callsigns are in which rooms,
// derived from ROOM JOIN and LEAVE envelopes (its own and those found in
// mailbox sweeps) plus node-local auto-subscription to the local room. It is
// safe for concurrent use.
type Subscriptions struct {
	local   envelope.Address
	expiry  uint32
	carries func(envelope.Address) bool

	mu    sync.Mutex
	calls map[string]*callRooms // by base callsign
}

type callRooms struct {
	lastRefreshed uint32
	rooms         map[envelope.Address]roomEntry
	auto          bool // auto-subscribed to the local room
}

// roomEntry is the latest JOIN or LEAVE seen for one room. joined=false is
// a sticky opt-out.
type roomEntry struct {
	ts     uint32
	joined bool
}

// NewSubscriptions returns empty subscription state.
func NewSubscriptions(cfg SubscriptionsConfig) (*Subscriptions, error) {
	s := &Subscriptions{expiry: cfg.Expiry, carries: cfg.Carries, calls: map[string]*callRooms{}}
	if s.expiry == 0 {
		s.expiry = DefaultSubscriptionExpiry
	}
	if cfg.NodeCallsign != "" {
		base, _ := envelope.SplitCallsign(strings.ToUpper(cfg.NodeCallsign))
		a, err := envelope.RoomAddress(base)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrNoLocalRoom, cfg.NodeCallsign, err)
		}
		s.local = a
	}
	return s, nil
}

// LocalRoom is the node-callsign room, or AddressZero if there is none.
func (s *Subscriptions) LocalRoom() envelope.Address { return s.local }

// Carries reports whether the node carries a room: it must be a valid room
// address and pass the configured filter. The local room is always carried.
func (s *Subscriptions) Carries(room envelope.Address) bool {
	if !room.IsRoom() {
		return false
	}
	if room == s.local || s.carries == nil {
		return true
	}
	return s.carries(room)
}

func baseOf(a envelope.Address) (string, error) {
	base := a.BaseCallsign()
	if base == "" {
		return "", fmt.Errorf("%w: %s", ErrNotCallsign, a)
	}
	return base, nil
}

// get returns the state for a base callsign, creating it if needed. Caller
// holds s.mu.
func (s *Subscriptions) get(base string) *callRooms {
	c := s.calls[base]
	if c == nil {
		c = &callRooms{rooms: map[envelope.Address]roomEntry{}}
		s.calls[base] = c
	}
	return c
}

// apply records one JOIN or LEAVE event. Latest timestamp wins; on a tie
// LEAVE wins (rooms spec §4.6). Caller holds s.mu.
func (c *callRooms) apply(room envelope.Address, ts uint32, joined bool) {
	cur, ok := c.rooms[room]
	if ok && (cur.ts > ts || (cur.ts == ts && !cur.joined)) {
		return
	}
	c.rooms[room] = roomEntry{ts: ts, joined: joined}
	if ts > c.lastRefreshed {
		c.lastRefreshed = ts
	}
}

// Apply records a stored ROOM JOIN or LEAVE envelope for a callsign, as
// found by a mailbox sweep or produced by Handle or ImplicitJoin on this
// node. The envelope's timestamp must be non-zero.
func (s *Subscriptions) Apply(callsign envelope.Address, e *envelope.Envelope) error {
	base, err := baseOf(callsign)
	if err != nil {
		return err
	}
	r, ok := e.Room()
	if !ok || (r.Op() != envelope.OpJoin && r.Op() != envelope.OpLeave) {
		return fmt.Errorf("%w: %s", ErrNotRoomOp, e)
	}
	if e.Timestamp() == 0 {
		return ErrNoTimestamp
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(base)
	for _, room := range r.Rooms() {
		c.apply(room, e.Timestamp(), r.Op() == envelope.OpJoin)
	}
	return nil
}

// Heard refreshes a callsign's subscriptions and auto-subscribes it to the
// local room unless it has opted out (rooms spec §4.4). Call it whenever the
// node hears the callsign on RF or from a directly connected client.
func (s *Subscriptions) Heard(callsign envelope.Address, now uint32) error {
	base, err := baseOf(callsign)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(base)
	if now > c.lastRefreshed {
		c.lastRefreshed = now
	}
	if s.local != envelope.AddressZero {
		cur, ok := c.rooms[s.local]
		c.auto = !ok || cur.joined
	}
	return nil
}

// ImplicitJoin records that a callsign sent a MSG to a room (rooms spec
// §4.2) and returns the synthesized ROOM JOIN to store in its mailbox. ts is
// the MSG's timestamp; now is used when it is 0.
func (s *Subscriptions) ImplicitJoin(callsign, room envelope.Address, ts, now uint32) (*envelope.Envelope, error) {
	if !room.IsRoom() {
		return nil, fmt.Errorf("%w: %s", ErrNotValidRoom, room)
	}
	if ts == 0 {
		ts = now
	}
	join, err := envelope.BuildRoom(envelope.OpJoin, ts, []envelope.Address{room}, "")
	if err != nil {
		return nil, fmt.Errorf("qtcd: implicit join: %w", err)
	}
	if err := s.Apply(callsign, join); err != nil {
		return nil, err
	}
	return join, nil
}

// Handle processes a ROOM request (JOIN, LEAVE, LIST) from callsign and
// returns the reply to send and, for JOIN and LEAVE, the envelope to PUT to
// the callsign's mailbox (nil if nothing was accepted). A request whose
// timestamp is 0 has now substituted before it is applied and stored. Rooms
// outside the valid range, or not carried by this node, are refused; a JOIN
// or LEAVE that is partly refused still applies the rest and replies
// REFUSED listing the refused rooms (rooms spec §5).
func (s *Subscriptions) Handle(callsign envelope.Address, req *envelope.Envelope, now uint32) (reply, store *envelope.Envelope, err error) {
	base, err := baseOf(callsign)
	if err != nil {
		return nil, nil, err
	}
	r, ok := req.Room()
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRoomOp, req)
	}
	if r.Op().IsReply() {
		return nil, nil, ErrNotRequest
	}
	ts := req.Timestamp()
	if ts == 0 {
		ts = now
	}
	switch r.Op() {
	case envelope.OpList:
		reply, err = envelope.BuildRoom(envelope.OpOK, now, s.rooms(base), "")
		return reply, nil, err
	case envelope.OpJoin, envelope.OpLeave:
	default:
		return nil, nil, fmt.Errorf("qtcd: unknown ROOM op %s", r.Op())
	}

	join := r.Op() == envelope.OpJoin
	var accepted, refused []envelope.Address
	for _, room := range r.Rooms() {
		if !room.IsRoom() || (join && !s.Carries(room)) {
			refused = append(refused, room)
		} else {
			accepted = append(accepted, room)
		}
	}
	if len(r.Rooms()) == 0 {
		return s.refused(now, nil, "no rooms listed")
	}
	if len(accepted) > 0 {
		s.mu.Lock()
		c := s.get(base)
		for _, room := range accepted {
			c.apply(room, ts, join)
		}
		if ts > c.lastRefreshed {
			c.lastRefreshed = ts
		}
		s.mu.Unlock()
		// Rebuild with the substituted timestamp and only the accepted
		// rooms. ROOM is not content-addressed, so this is permitted
		// (rooms spec §5.1).
		store, err = envelope.BuildRoom(r.Op(), ts, accepted, "")
		if err != nil {
			return nil, nil, fmt.Errorf("qtcd: store envelope: %w", err)
		}
	}
	if len(refused) > 0 {
		note := "not a room"
		if join {
			note = "room not carried"
		}
		reply, err = s.refusedReply(now, refused, note)
		return reply, store, err
	}
	reply, err = envelope.BuildRoom(envelope.OpOK, now, nil, "")
	return reply, store, err
}

func (s *Subscriptions) refused(now uint32, rooms []envelope.Address, note string) (*envelope.Envelope, *envelope.Envelope, error) {
	reply, err := s.refusedReply(now, rooms, note)
	return reply, nil, err
}

func (s *Subscriptions) refusedReply(now uint32, rooms []envelope.Address, note string) (*envelope.Envelope, error) {
	reply, err := envelope.BuildRoom(envelope.OpRefused, now, rooms, note)
	if err != nil {
		return nil, fmt.Errorf("qtcd: REFUSED reply: %w", err)
	}
	return reply, nil
}

// rooms returns the sorted rooms a base callsign is currently in,
// auto-subscription included.
func (s *Subscriptions) rooms(base string) []envelope.Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls[base]
	if c == nil {
		return nil
	}
	var out []envelope.Address
	for room, e := range c.rooms {
		if e.joined {
			out = append(out, room)
		}
	}
	if c.auto {
		if e, ok := c.rooms[s.local]; !ok || !e.joined {
			out = append(out, s.local)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Rooms returns the rooms a callsign is subscribed to, sorted by address.
func (s *Subscriptions) Rooms(callsign envelope.Address) ([]envelope.Address, error) {
	base, err := baseOf(callsign)
	if err != nil {
		return nil, err
	}
	return s.rooms(base), nil
}

// Subscribers returns the base callsigns subscribed to a room, sorted.
func (s *Subscriptions) Subscribers(room envelope.Address) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for base, c := range s.calls {
		if c.in(room, s.local) {
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out
}

func (c *callRooms) in(room, local envelope.Address) bool {
	if e, ok := c.rooms[room]; ok {
		return e.joined
	}
	return room == local && c.auto
}

// ActiveRooms returns every room with at least one local subscriber, sorted.
// It is the set of gossipsub topics the node should hold.
func (s *Subscriptions) ActiveRooms() []envelope.Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[envelope.Address]bool{}
	for _, c := range s.calls {
		for room, e := range c.rooms {
			if e.joined {
				set[room] = true
			}
		}
		if c.auto {
			set[s.local] = true
		}
	}
	out := make([]envelope.Address, 0, len(set))
	for room := range set {
		out = append(out, room)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Expire drops every callsign whose last refresh is older than the expiry
// (rooms spec §4.5), opt-outs included, and returns the dropped base
// callsigns sorted.
func (s *Subscriptions) Expire(now uint32) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for base, c := range s.calls {
		if now > c.lastRefreshed && now-c.lastRefreshed > s.expiry {
			delete(s.calls, base)
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out
}

// ParseRoomCommand parses a legacy SMS room command (rooms spec §6):
// "/join NAME [NAME…]", "/leave NAME [NAME…]", or "/rooms". Matching is
// case-insensitive, and a room name may carry the optional '#' prefix. It
// returns ErrNotCommand if the text does not begin with '/', so the caller
// can treat it as a message; ErrUnknownCmd for any other command; and
// ErrBadRoomName for a name outside §3.1.
func ParseRoomCommand(text string) (envelope.RoomOp, []envelope.Address, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return 0, nil, ErrNotCommand
	}
	fields := strings.Fields(text[1:])
	if len(fields) == 0 {
		return 0, nil, fmt.Errorf("%w: %q", ErrUnknownCmd, text)
	}
	var op envelope.RoomOp
	switch strings.ToLower(fields[0]) {
	case "join":
		op = envelope.OpJoin
	case "leave":
		op = envelope.OpLeave
	case "rooms":
		if len(fields) > 1 {
			return 0, nil, fmt.Errorf("%w: /rooms takes no arguments", ErrUnknownCmd)
		}
		return envelope.OpList, nil, nil
	default:
		return 0, nil, fmt.Errorf("%w: %q", ErrUnknownCmd, fields[0])
	}
	if len(fields) == 1 {
		return 0, nil, fmt.Errorf("%w: /%s needs at least one room", ErrBadRoomName, strings.ToLower(fields[0]))
	}
	rooms := make([]envelope.Address, 0, len(fields)-1)
	for _, name := range fields[1:] {
		a, err := envelope.RoomAddress(strings.TrimPrefix(name, "#"))
		if err != nil {
			return 0, nil, fmt.Errorf("%w: %v", ErrBadRoomName, err)
		}
		rooms = append(rooms, a)
	}
	return op, rooms, nil
}
