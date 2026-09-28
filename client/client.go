// Package client is the device side of the QTC native client protocol
// (docs/qtc-client.md), independent of transport: qtc chat runs it over
// M17_inet, qtc-radio over RF through a modem.
//
// A Session sends MSGs and keeps resending them until the node acknowledges
// them, acknowledges what it receives (DELIVERED for a message to a
// callsign, ACK for a room message), syncs what it missed from a saved
// cursor, and, on RF, answers room summaries with FETCH. The caller feeds it
// received packets with Receive and the time with Tick, gives it a Send
// function for outgoing packets, and gets what happened as Events.
//
// The package imports only envelope and the standard library.
package client

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
)

// Config configures a Session. Zero durations and counts take the defaults
// for the link: RF when RF is set, M17_inet otherwise (client spec §4.3).
type Config struct {
	// Me is this device's callsign, suffix included if it has one.
	Me envelope.Address
	// RF means the device is on the air: it hears packets for other
	// devices and filters them, room messages are not acknowledged outside
	// a sync page, and room summaries are answered with FETCH.
	RF bool

	AckTimeout time.Duration // resend an unacknowledged MSG after this
	AckJitter  time.Duration // plus a random 0 to AckJitter
	AckRetries int           // resends before giving up
	PageQuiet  time.Duration // a sync page is done after this long with nothing new
	FetchDelay time.Duration // RF: wait a random 0 to FetchDelay after a summary before FETCH

	// StatePath keeps the sync position between runs; "" keeps nothing.
	StatePath string

	// Send transmits a QTC payload with the given LSF destination.
	Send func(dst envelope.Address, e *envelope.Envelope)
	// Event reports what happened. It is called without the session's lock
	// held, from whichever goroutine called Receive, Tick, or a send method.
	Event func(Event)
}

func (c *Config) defaults() {
	if c.AckTimeout == 0 {
		c.AckTimeout = 2 * time.Second
		if c.RF {
			c.AckTimeout = 5 * time.Second
		}
	}
	if c.AckJitter == 0 && c.RF {
		c.AckJitter = 2 * time.Second
	}
	if c.AckRetries == 0 {
		c.AckRetries = 3
	}
	if c.PageQuiet == 0 {
		c.PageQuiet = 3 * time.Second
		if c.RF {
			c.PageQuiet = 8 * time.Second
		}
	}
	if c.FetchDelay == 0 {
		c.FetchDelay = 3 * time.Second
	}
}

// EventKind says what an Event reports.
type EventKind int

const (
	// EventMessage: a MSG for us, shown once however often it comes. Env
	// is the MSG; FromNode is set when the node itself sent it.
	EventMessage EventKind = iota
	// EventReceipt: a RCPT about something we sent. Text is what we sent,
	// if this session sent it.
	EventReceipt
	// EventRoomReply: the node's reply to a ROOM request. Op is the
	// request's op; Env the reply.
	EventRoomReply
	// EventNotSent: a MSG the node never acknowledged. Text is its body.
	EventNotSent
	// EventSyncRefused: the node refused a sync. Text is its reason.
	EventSyncRefused
	// EventSyncDone: a sync finished.
	EventSyncDone
)

// Event is something that happened in a Session.
type Event struct {
	Kind     EventKind
	Env      *envelope.Envelope
	FromNode bool
	Op       envelope.RoomOp
	Text     string
}

// ErrNotLinked is returned by the send methods before Linked.
var ErrNotLinked = errors.New("not linked")

// Session is one device's native-protocol state. Its methods are safe for
// concurrent use.
type Session struct {
	cfg Config

	mu       sync.Mutex
	linked   bool
	node     envelope.Address // zero until learned; control packets then go to broadcast
	pending  map[envelope.ID]*outgoing
	sentText map[envelope.ID]string
	seen     map[envelope.ID]bool
	seenS    map[envelope.ShortID]bool
	roomReqs []roomReq
	rooms    map[envelope.Address]bool
	known    bool // rooms is the node's answer, not a guess
	sync     syncState
	fetch    fetchState
	events   []Event // queued under mu, delivered after it is released
}

type outgoing struct {
	e        *envelope.Envelope
	attempts int
	next     time.Time
}

type roomReq struct {
	op    envelope.RoomOp
	rooms []envelope.Address
}

// syncState is the sync in progress (client spec §5).
type syncState struct {
	cursor    uint32
	skip      uint16
	active    bool
	requested time.Time // last REQUEST, until its PAGE; zero otherwise
	expect    int       // page packets still to come
	remaining uint16
	lastPkt   time.Time
	next      struct {
		cursor uint32
		skip   uint16
	}
}

// fetchState is a FETCH waiting to go out after a summary (client spec
// §6.1).
type fetchState struct {
	want     map[envelope.ShortID]bool
	at       time.Time // zero: nothing scheduled
	attempts int
}

// New returns a Session, with the sync position loaded from StatePath.
func New(cfg Config) *Session {
	cfg.defaults()
	s := &Session{
		cfg:      cfg,
		pending:  map[envelope.ID]*outgoing{},
		sentText: map[envelope.ID]string{},
		seen:     map[envelope.ID]bool{},
		seenS:    map[envelope.ShortID]bool{},
		rooms:    map[envelope.Address]bool{},
		fetch:    fetchState{want: map[envelope.ShortID]bool{}},
	}
	s.loadState()
	return s
}

// DefaultStatePath is where a device's sync position is kept: per
// callsign, since a cursor is good at any node (client spec §5.2).
func DefaultStatePath(me envelope.Address) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	name := strings.NewReplacer("/", "_", " ", "_").Replace(strings.TrimSpace(me.String()))
	return filepath.Join(dir, "qtc", "sync-"+name+".json")
}

type savedState struct {
	Cursor uint32 `json:"cursor"`
	Skip   uint16 `json:"skip"`
}

func (s *Session) loadState() {
	if s.cfg.StatePath == "" {
		return
	}
	b, err := os.ReadFile(s.cfg.StatePath)
	if err != nil {
		return
	}
	var st savedState
	if json.Unmarshal(b, &st) == nil {
		s.sync.cursor, s.sync.skip = st.Cursor, st.Skip
	}
}

// saveState records the sync position. Callers hold s.mu.
func (s *Session) saveState() {
	if s.cfg.StatePath == "" {
		return
	}
	b, _ := json.Marshal(savedState{s.sync.cursor, s.sync.skip})
	if err := os.MkdirAll(filepath.Dir(s.cfg.StatePath), 0o700); err == nil {
		os.WriteFile(s.cfg.StatePath, b, 0o600)
	}
}

// emit queues an event. Callers hold s.mu and call flush after unlocking.
func (s *Session) emit(ev Event) { s.events = append(s.events, ev) }

func (s *Session) unlockAndFlush() {
	evs := s.events
	s.events = nil
	s.mu.Unlock()
	if s.cfg.Event != nil {
		for _, ev := range evs {
			s.cfg.Event(ev)
		}
	}
}

// Node is the node's callsign, once known.
func (s *Session) Node() envelope.Address {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.node
}

// SetNode records the node's callsign, e.g. from its M17_inet ACKN.
func (s *Session) SetNode(a envelope.Address) {
	if !a.IsStandard() {
		return
	}
	s.mu.Lock()
	s.node = a
	s.mu.Unlock()
}

// Linked says the device can reach its node, and starts a sync.
func (s *Session) Linked() {
	s.mu.Lock()
	s.linked = true
	s.mu.Unlock()
	s.StartSync()
}

// Unlinked says the link is down; sends are refused until Linked.
func (s *Session) Unlinked() {
	s.mu.Lock()
	s.linked = false
	s.mu.Unlock()
}

// controlDst is where control packets go: the node, or broadcast until
// its callsign is known (client spec §2). Callers hold s.mu.
func (s *Session) controlDst() envelope.Address {
	if s.node == 0 {
		return envelope.Broadcast
	}
	return s.node
}

// SendMsg sends text to a callsign or room, asking a callsign for
// receipts, and resends it until the node acknowledges it.
func (s *Session) SendMsg(dst envelope.Address, text string) (*envelope.Envelope, error) {
	if len(text) > envelope.MaxBodyUnsigned {
		return nil, errors.New("message too long")
	}
	var flags byte
	if !dst.IsRoom() {
		flags = envelope.FlagRcptReq
	}
	nonce, err := envelope.NewNonce()
	if err != nil {
		return nil, err
	}
	e, err := envelope.BuildMsg(s.cfg.Me, dst, uint32(time.Now().Unix()), envelope.TTLDefault, nonce, flags, text)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if !s.linked {
		s.mu.Unlock()
		return nil, ErrNotLinked
	}
	s.pending[e.ID()] = &outgoing{e: e, attempts: 1, next: time.Now().Add(s.ackWait())}
	s.sentText[e.ID()] = text
	s.markSeen(e) // our own room posts are not shown back
	if dst.IsRoom() {
		s.rooms[dst] = true // sending to a room joins it
	}
	s.mu.Unlock()
	s.cfg.Send(dst, e)
	return e, nil
}

// RoomRequest sends a ROOM JOIN, LEAVE, or LIST.
func (s *Session) RoomRequest(op envelope.RoomOp, rooms []envelope.Address) error {
	req, err := envelope.BuildRoom(op, uint32(time.Now().Unix()), rooms, "")
	if err != nil {
		return err
	}
	s.mu.Lock()
	if !s.linked {
		s.mu.Unlock()
		return ErrNotLinked
	}
	s.roomReqs = append(s.roomReqs, roomReq{op, rooms})
	dst := s.controlDst()
	s.mu.Unlock()
	s.cfg.Send(dst, req)
	return nil
}

// StartSync asks for what was missed since the saved position.
func (s *Session) StartSync() {
	s.mu.Lock()
	s.sync.active = true
	cursor, skip := s.sync.cursor, s.sync.skip
	s.mu.Unlock()
	s.requestPage(cursor, skip)
}

func (s *Session) requestPage(cursor uint32, skip uint16) {
	req, err := envelope.BuildSync(envelope.SyncRequest, 0, cursor, skip, 0, 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.sync.requested = time.Now()
	s.sync.lastPkt = time.Time{}
	s.sync.expect = 0
	dst := s.controlDst()
	s.mu.Unlock()
	s.cfg.Send(dst, req)
}

// ackWait is the time to wait for an acknowledgement. Callers hold s.mu.
func (s *Session) ackWait() time.Duration {
	return s.cfg.AckTimeout + time.Duration(rand.Int64N(int64(s.cfg.AckJitter)+1))
}

// markSeen notes a MSG as had. Callers hold s.mu.
func (s *Session) markSeen(e *envelope.Envelope) {
	s.seen[e.ID()] = true
	s.seenS[e.ID().Short()] = true
	delete(s.fetch.want, e.ID().Short())
}

// forMe reports whether a MSG is one this device should take: on the air
// a radio hears everything on the channel. Callers hold s.mu.
func (s *Session) forMe(e *envelope.Envelope) bool {
	dst := e.Destination()
	switch {
	case dst == s.cfg.Me || dst == s.cfg.Me.Base():
		return true
	case dst.IsRoom():
		return !s.cfg.RF || !s.known || s.rooms[dst]
	}
	return false
}

// Receive handles a QTC payload with its LSF addresses.
func (s *Session) Receive(lsfDst, lsfSrc envelope.Address, e *envelope.Envelope) {
	switch e.Kind() {
	case envelope.KindMSG:
		s.receiveMsg(e)
	case envelope.KindRCPT:
		s.receiveRcpt(e)
	case envelope.KindACK, envelope.KindROOM, envelope.KindSYNC:
		if y, ok := e.Sync(); ok && y.Op() == envelope.SyncSummary {
			s.receiveSummary(lsfSrc, y)
			return
		}
		if s.cfg.RF && lsfDst != s.cfg.Me {
			return // another radio's
		}
		s.mu.Lock()
		if lsfSrc.IsStandard() {
			s.node = lsfSrc
		}
		s.mu.Unlock()
		switch e.Kind() {
		case envelope.KindACK:
			a, _ := e.Ack()
			s.mu.Lock()
			for _, id := range a.IDs() {
				delete(s.pending, id)
			}
			s.mu.Unlock()
		case envelope.KindROOM:
			s.receiveRoom(e)
		case envelope.KindSYNC:
			y, _ := e.Sync()
			s.receiveSync(y)
		}
	}
}

// receiveMsg shows a message once and acknowledges it every time:
// DELIVERED for a callsign, ACK for a room, except that on the air a room
// message is acknowledged only in a sync page (client spec §4).
func (s *Session) receiveMsg(e *envelope.Envelope) {
	s.mu.Lock()
	if !s.forMe(e) {
		s.mu.Unlock()
		return
	}
	inPage := s.sync.active && s.sync.expect > 0
	if inPage {
		s.sync.expect--
		s.sync.lastPkt = time.Now()
	}
	dup := s.seen[e.ID()]
	s.markSeen(e)
	fromNode := s.node != 0 && e.Source() == s.node
	if !dup {
		s.emit(Event{Kind: EventMessage, Env: e, FromNode: fromNode})
	}
	ackDst := s.controlDst()
	s.unlockAndFlush()

	switch {
	case !e.Destination().IsRoom():
		dl, err := envelope.BuildRcpt(s.cfg.Me, e.Source(), e.ID(), envelope.StatusDelivered, uint32(time.Now().Unix()), 0, "")
		if err == nil {
			s.cfg.Send(e.Source(), dl)
		}
	case !s.cfg.RF || inPage:
		if ack, err := envelope.BuildAck([]envelope.ID{e.ID()}); err == nil {
			s.cfg.Send(ackDst, ack)
		}
	}
}

// receiveRcpt reports a receipt for something this callsign sent.
func (s *Session) receiveRcpt(e *envelope.Envelope) {
	rc, _ := e.Rcpt()
	s.mu.Lock()
	if e.Destination().Base() != s.cfg.Me.Base() {
		s.mu.Unlock()
		return
	}
	if s.sync.active && s.sync.expect > 0 {
		s.sync.expect--
		s.sync.lastPkt = time.Now()
	}
	if rc.Status() == envelope.StatusRejected {
		delete(s.pending, rc.MessageID())
	}
	s.emit(Event{Kind: EventReceipt, Env: e, Text: s.sentText[rc.MessageID()]})
	s.unlockAndFlush()
}

// receiveRoom reports the reply to the oldest ROOM request, and keeps the
// set of rooms the callsign is in.
func (s *Session) receiveRoom(e *envelope.Envelope) {
	r, _ := e.Room()
	s.mu.Lock()
	req := roomReq{op: envelope.OpList}
	if len(s.roomReqs) > 0 {
		req, s.roomReqs = s.roomReqs[0], s.roomReqs[1:]
	}
	if r.Op() == envelope.OpOK {
		switch req.op {
		case envelope.OpList:
			s.rooms = map[envelope.Address]bool{}
			for _, a := range r.Rooms() {
				s.rooms[a] = true
			}
			s.known = true
		case envelope.OpJoin:
			for _, a := range req.rooms {
				s.rooms[a] = true
			}
		case envelope.OpLeave:
			for _, a := range req.rooms {
				delete(s.rooms, a)
			}
		}
	}
	s.emit(Event{Kind: EventRoomReply, Env: e, Op: req.op})
	s.unlockAndFlush()
}

func (s *Session) receiveSync(y envelope.Sync) {
	switch y.Op() {
	case envelope.SyncPage:
		s.mu.Lock()
		s.sync.active = true
		s.sync.requested = time.Time{}
		s.sync.expect = y.Count()
		s.sync.remaining = y.Remaining()
		s.sync.next.cursor, s.sync.next.skip = y.Cursor(), y.Skip()
		s.sync.lastPkt = time.Now()
		s.mu.Unlock()
	case envelope.SyncNotify:
		s.mu.Lock()
		busy := s.sync.active
		s.mu.Unlock()
		if !busy {
			s.StartSync()
		}
	case envelope.SyncRefused:
		s.mu.Lock()
		s.sync.active = false
		s.emit(Event{Kind: EventSyncRefused, Text: y.Note()})
		s.unlockAndFlush()
	}
}

// receiveSummary schedules a FETCH for listed room messages this device
// has not had, in rooms it is in (client spec §6.1).
func (s *Session) receiveSummary(src envelope.Address, y envelope.Sync) {
	if !s.cfg.RF {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if src.IsStandard() && s.node == 0 {
		s.node = src
	}
	for _, g := range y.Groups() {
		if s.known && !s.rooms[g.Room] {
			continue
		}
		for _, id := range g.IDs {
			if !s.seenS[id] {
				s.fetch.want[id] = true
			}
		}
	}
	if len(s.fetch.want) > 0 && s.fetch.at.IsZero() {
		s.fetch.attempts = 0
		s.fetch.at = time.Now().Add(time.Duration(rand.Int64N(int64(s.cfg.FetchDelay) + 1)))
	}
}

// Tick resends unacknowledged MSGs, moves a sync along, and sends a due
// FETCH. Call it a few times a second.
func (s *Session) Tick(now time.Time) {
	s.retry(now)
	s.syncTick(now)
	s.fetchTick(now)
}

func (s *Session) retry(now time.Time) {
	var resend []*envelope.Envelope
	s.mu.Lock()
	for id, o := range s.pending {
		if now.Before(o.next) {
			continue
		}
		if o.attempts > s.cfg.AckRetries {
			delete(s.pending, id)
			s.emit(Event{Kind: EventNotSent, Env: o.e, Text: s.sentText[id]})
			continue
		}
		o.attempts++
		o.next = now.Add(s.ackWait())
		resend = append(resend, o.e)
	}
	s.unlockAndFlush()
	for _, e := range resend {
		s.cfg.Send(e.Destination(), e)
	}
}

// syncTick: a page is done when all its packets have come or nothing has
// come for PageQuiet; then the next page, or the end. A REQUEST with no
// PAGE is repeated.
func (s *Session) syncTick(now time.Time) {
	s.mu.Lock()
	st := s.sync
	if !st.active {
		s.mu.Unlock()
		return
	}
	var cursor uint32
	var skip uint16
	request := false
	switch {
	case !st.requested.IsZero():
		if now.Sub(st.requested) > s.cfg.PageQuiet {
			cursor, skip, request = st.cursor, st.skip, true
		}
	case !st.lastPkt.IsZero() && (st.expect == 0 || now.Sub(st.lastPkt) > s.cfg.PageQuiet):
		// Let the last packet's acknowledgement go out before asking
		// again; on the air, give the channel a moment.
		if st.expect == 0 && s.cfg.RF && now.Sub(st.lastPkt) < time.Second {
			break
		}
		s.sync.cursor, s.sync.skip = st.next.cursor, st.next.skip
		s.saveState()
		if st.remaining == 0 {
			s.sync.active = false
			s.emit(Event{Kind: EventSyncDone})
		} else {
			cursor, skip, request = st.next.cursor, st.next.skip, true
		}
	}
	s.unlockAndFlush()
	if request {
		s.requestPage(cursor, skip)
	}
}

func (s *Session) fetchTick(now time.Time) {
	s.mu.Lock()
	if s.fetch.at.IsZero() || now.Before(s.fetch.at) {
		s.mu.Unlock()
		return
	}
	var ids []envelope.ShortID
	for id := range s.fetch.want {
		ids = append(ids, id)
	}
	if len(ids) == 0 || s.fetch.attempts > s.cfg.AckRetries {
		// Everything came, or it is left to the next sync.
		s.fetch = fetchState{want: map[envelope.ShortID]bool{}}
		s.mu.Unlock()
		return
	}
	s.fetch.attempts++
	s.fetch.at = now.Add(s.ackWait())
	dst := s.controlDst()
	s.mu.Unlock()
	if len(ids) > 255 {
		ids = ids[:255]
	}
	if f, err := envelope.BuildSyncFetch(ids); err == nil {
		s.cfg.Send(dst, f)
	}
}
