package qtcd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jancona/qtc/envelope"
)

const t0 uint32 = 1789128000

func mustAddr(t *testing.T, text string) envelope.Address {
	t.Helper()
	a, err := envelope.EncodeAddress(text)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustRoom(t *testing.T, name string) envelope.Address {
	t.Helper()
	a, err := envelope.RoomAddress(name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustRoomPkt(t *testing.T, op envelope.RoomOp, ts uint32, rooms ...envelope.Address) *envelope.Envelope {
	t.Helper()
	e, err := envelope.BuildRoom(op, ts, rooms, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func newSubs(t *testing.T, cfg SubscriptionsConfig) *Subscriptions {
	t.Helper()
	s, err := NewSubscriptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func roomsOf(t *testing.T, s *Subscriptions, call envelope.Address) []envelope.Address {
	t.Helper()
	r, err := s.Rooms(call)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewSubscriptionsLocalRoom(t *testing.T) {
	tests := []struct {
		node string
		want string // room name; "" = no local room
		err  bool
	}{
		{"K1XYZ  R", "K1XYZ", false},
		{"n1adj", "N1ADJ", false},
		{"N1ADJ-7", "N1ADJ", false},
		{"", "", false},
		{"VE3/N1ADJ", "", true}, // '/' is not a room character
	}
	for _, tt := range tests {
		s, err := NewSubscriptions(SubscriptionsConfig{NodeCallsign: tt.node})
		if tt.err {
			if !errors.Is(err, ErrNoLocalRoom) {
				t.Errorf("%q: err = %v, want ErrNoLocalRoom", tt.node, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tt.node, err)
		}
		if tt.want == "" {
			if s.LocalRoom() != envelope.AddressZero {
				t.Errorf("%q: local room %s, want none", tt.node, s.LocalRoom())
			}
			continue
		}
		if s.LocalRoom() != mustRoom(t, tt.want) {
			t.Errorf("%q: local room %s, want #%s", tt.node, s.LocalRoom(), tt.want)
		}
	}
}

func TestHandleJoinLeaveList(t *testing.T) {
	maine, dev, k1xyz := mustRoom(t, "MAINE"), mustRoom(t, "M17DEV"), mustRoom(t, "K1XYZ")
	bad := envelope.ReservedRooms
	n1adj := mustAddr(t, "N1ADJ  H")
	notCarried := mustRoom(t, "SECRET")
	s := newSubs(t, SubscriptionsConfig{NodeCallsign: "K1XYZ  R", Carries: func(a envelope.Address) bool { return a != notCarried }})

	tests := []struct {
		name      string
		req       *envelope.Envelope
		now       uint32
		wantOp    envelope.RoomOp
		wantRooms []envelope.Address // in the reply
		wantStore *envelope.Envelope // nil = nothing stored
		wantSubs  []envelope.Address // after the request
	}{
		{"join two", mustRoomPkt(t, envelope.OpJoin, t0, maine, dev), t0 + 1,
			envelope.OpOK, nil, mustRoomPkt(t, envelope.OpJoin, t0, maine, dev), []envelope.Address{maine, dev}},
		{"join no clock substitutes now", mustRoomPkt(t, envelope.OpJoin, 0, k1xyz), t0 + 2,
			envelope.OpOK, nil, mustRoomPkt(t, envelope.OpJoin, t0+2, k1xyz), []envelope.Address{maine, dev, k1xyz}},
		{"list", mustRoomPkt(t, envelope.OpList, t0+3), t0 + 3,
			envelope.OpOK, []envelope.Address{maine, dev, k1xyz}, nil, []envelope.Address{maine, dev, k1xyz}},
		{"leave one", mustRoomPkt(t, envelope.OpLeave, t0+4, dev), t0 + 4,
			envelope.OpOK, nil, mustRoomPkt(t, envelope.OpLeave, t0+4, dev), []envelope.Address{maine, k1xyz}},
		{"leave room not in still ok", mustRoomPkt(t, envelope.OpLeave, t0+5, mustRoom(t, "NEVER")), t0 + 5,
			envelope.OpOK, nil, mustRoomPkt(t, envelope.OpLeave, t0+5, mustRoom(t, "NEVER")), []envelope.Address{maine, k1xyz}},
		{"join partly refused", mustRoomPkt(t, envelope.OpJoin, t0+6, dev, bad, notCarried), t0 + 6,
			envelope.OpRefused, []envelope.Address{bad, notCarried}, mustRoomPkt(t, envelope.OpJoin, t0+6, dev), []envelope.Address{maine, dev, k1xyz}},
		{"join all refused", mustRoomPkt(t, envelope.OpJoin, t0+7, bad), t0 + 7,
			envelope.OpRefused, []envelope.Address{bad}, nil, []envelope.Address{maine, dev, k1xyz}},
		{"join empty refused", mustRoomPkt(t, envelope.OpJoin, t0+8), t0 + 8,
			envelope.OpRefused, nil, nil, []envelope.Address{maine, dev, k1xyz}},
		{"leave invalid refused", mustRoomPkt(t, envelope.OpLeave, t0+9, envelope.Broadcast), t0 + 9,
			envelope.OpRefused, []envelope.Address{envelope.Broadcast}, nil, []envelope.Address{maine, dev, k1xyz}},
		{"leave local room", mustRoomPkt(t, envelope.OpLeave, t0+10, k1xyz), t0 + 10,
			envelope.OpOK, nil, mustRoomPkt(t, envelope.OpLeave, t0+10, k1xyz), []envelope.Address{maine, dev}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply, store, err := s.Handle(n1adj, tt.req, tt.now)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			r, _ := reply.Room()
			if r.Op() != tt.wantOp {
				t.Errorf("reply op %s, want %s (%s)", r.Op(), tt.wantOp, reply)
			}
			if reply.Timestamp() != tt.now {
				t.Errorf("reply ts %d, want %d", reply.Timestamp(), tt.now)
			}
			if got := r.Rooms(); !sameAddrs(got, tt.wantRooms) {
				t.Errorf("reply rooms %v, want %v", got, tt.wantRooms)
			}
			if tt.wantOp == envelope.OpRefused && r.Note() == "" {
				t.Error("REFUSED without a note")
			}
			switch {
			case tt.wantStore == nil && store != nil:
				t.Errorf("stored %s, want nothing", store)
			case tt.wantStore != nil && (store == nil || !reflect.DeepEqual(store.Bytes(), tt.wantStore.Bytes())):
				t.Errorf("stored %v, want %s", store, tt.wantStore)
			}
			if got := roomsOf(t, s, n1adj); !sameAddrs(got, tt.wantSubs) {
				t.Errorf("subscriptions %v, want %v", got, tt.wantSubs)
			}
		})
	}

	// Subscriptions are per base callsign: the P device sees the same list.
	if got := roomsOf(t, s, mustAddr(t, "N1ADJ  P")); !sameAddrs(got, []envelope.Address{maine, dev}) {
		t.Errorf("other device sees %v", got)
	}
	if got := s.Subscribers(maine); !reflect.DeepEqual(got, []string{"N1ADJ"}) {
		t.Errorf("Subscribers(MAINE) = %v", got)
	}
	if got := s.ActiveRooms(); !sameAddrs(got, []envelope.Address{maine, dev}) {
		t.Errorf("ActiveRooms = %v", got)
	}

	// Replies and non-ROOM envelopes are rejected.
	if _, _, err := s.Handle(n1adj, mustRoomPkt(t, envelope.OpOK, t0), t0); !errors.Is(err, ErrNotRequest) {
		t.Errorf("Handle(OK) err = %v", err)
	}
	msg, _ := envelope.BuildMsg(n1adj, maine, t0, 60, 1, 0, "hi")
	if _, _, err := s.Handle(n1adj, msg, t0); !errors.Is(err, ErrNotRoomOp) {
		t.Errorf("Handle(MSG) err = %v", err)
	}
	if _, _, err := s.Handle(maine, mustRoomPkt(t, envelope.OpList, t0), t0); !errors.Is(err, ErrNotCallsign) {
		t.Errorf("Handle from a room address err = %v", err)
	}
}

func TestAutoSubscriptionAndStickyLeave(t *testing.T) {
	local := mustRoom(t, "K1XYZ")
	w1aw := mustAddr(t, "W1AW")
	s := newSubs(t, SubscriptionsConfig{NodeCallsign: "K1XYZ  R"})

	if err := s.Heard(w1aw, t0); err != nil {
		t.Fatal(err)
	}
	if got := roomsOf(t, s, w1aw); !sameAddrs(got, []envelope.Address{local}) {
		t.Fatalf("after Heard: %v, want auto-subscribed to local room", got)
	}
	if got := s.Subscribers(local); !reflect.DeepEqual(got, []string{"W1AW"}) {
		t.Errorf("Subscribers(local) = %v", got)
	}

	// Explicit LEAVE is sticky: hearing the callsign again does not re-add it.
	if _, _, err := s.Handle(w1aw, mustRoomPkt(t, envelope.OpLeave, t0+1, local), t0+1); err != nil {
		t.Fatal(err)
	}
	if err := s.Heard(w1aw, t0+2); err != nil {
		t.Fatal(err)
	}
	if got := roomsOf(t, s, w1aw); len(got) != 0 {
		t.Errorf("after LEAVE and Heard: %v, want none", got)
	}
	if got := s.ActiveRooms(); len(got) != 0 {
		t.Errorf("ActiveRooms after opt-out = %v", got)
	}

	// Sending to the room (implicit join) clears the opt-out.
	join, err := s.ImplicitJoin(w1aw, local, t0+3, t0+3)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustRoomPkt(t, envelope.OpJoin, t0+3, local); !reflect.DeepEqual(join.Bytes(), want.Bytes()) {
		t.Errorf("ImplicitJoin envelope %s, want %s", join, want)
	}
	if got := roomsOf(t, s, w1aw); !sameAddrs(got, []envelope.Address{local}) {
		t.Errorf("after implicit join: %v", got)
	}

	// Implicit join with a clock-less MSG uses now.
	other := mustRoom(t, "MAINE")
	join, err = s.ImplicitJoin(w1aw, other, 0, t0+4)
	if err != nil {
		t.Fatal(err)
	}
	if join.Timestamp() != t0+4 {
		t.Errorf("implicit join ts %d, want %d", join.Timestamp(), t0+4)
	}
	if _, err := s.ImplicitJoin(w1aw, envelope.ReservedRooms, t0, t0); !errors.Is(err, ErrNotValidRoom) {
		t.Errorf("ImplicitJoin(reserved) err = %v", err)
	}

	// A node without a local room never auto-subscribes.
	none := newSubs(t, SubscriptionsConfig{})
	if err := none.Heard(w1aw, t0); err != nil {
		t.Fatal(err)
	}
	if got := roomsOf(t, none, w1aw); len(got) != 0 {
		t.Errorf("no-local-room node auto-subscribed: %v", got)
	}
	// The local room is not a JOIN candidate for the Carries filter.
	if !s.Carries(local) || s.Carries(envelope.ReservedRooms) || s.Carries(w1aw) {
		t.Error("Carries wrong")
	}
}

func TestApplyOrdering(t *testing.T) {
	maine := mustRoom(t, "MAINE")
	n1adj := mustAddr(t, "N1ADJ")
	join := func(ts uint32) *envelope.Envelope { return mustRoomPkt(t, envelope.OpJoin, ts, maine) }
	leave := func(ts uint32) *envelope.Envelope { return mustRoomPkt(t, envelope.OpLeave, ts, maine) }

	tests := []struct {
		name   string
		events []*envelope.Envelope // applied in this order, as a sweep might deliver them
		joined bool
	}{
		{"join then leave", []*envelope.Envelope{join(t0), leave(t0 + 1)}, false},
		{"leave then join", []*envelope.Envelope{leave(t0), join(t0 + 1)}, true},
		{"out of order: old leave after new join", []*envelope.Envelope{join(t0 + 1), leave(t0)}, true},
		{"out of order: old join after new leave", []*envelope.Envelope{leave(t0 + 1), join(t0)}, false},
		{"tie: leave wins, join first", []*envelope.Envelope{join(t0), leave(t0)}, false},
		{"tie: leave wins, leave first", []*envelope.Envelope{leave(t0), join(t0)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSubs(t, SubscriptionsConfig{})
			for _, e := range tt.events {
				if err := s.Apply(n1adj, e); err != nil {
					t.Fatal(err)
				}
			}
			got := roomsOf(t, s, n1adj)
			if (len(got) == 1) != tt.joined {
				t.Errorf("joined = %v, want %v (rooms %v)", len(got) == 1, tt.joined, got)
			}
		})
	}

	s := newSubs(t, SubscriptionsConfig{})
	if err := s.Apply(n1adj, join(0)); !errors.Is(err, ErrNoTimestamp) {
		t.Errorf("Apply(ts 0) err = %v, want ErrNoTimestamp", err)
	}
	if err := s.Apply(n1adj, mustRoomPkt(t, envelope.OpList, t0)); !errors.Is(err, ErrNotRoomOp) {
		t.Errorf("Apply(LIST) err = %v, want ErrNotRoomOp", err)
	}
	if err := s.Apply(n1adj, mustRoomPkt(t, envelope.OpOK, t0, maine)); !errors.Is(err, ErrNotRoomOp) {
		t.Errorf("Apply(OK) err = %v, want ErrNotRoomOp", err)
	}
}

func TestExpire(t *testing.T) {
	maine := mustRoom(t, "MAINE")
	n1adj, w1aw := mustAddr(t, "N1ADJ"), mustAddr(t, "W1AW")
	s := newSubs(t, SubscriptionsConfig{Expiry: 100})
	if err := s.Apply(n1adj, mustRoomPkt(t, envelope.OpJoin, t0, maine)); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(w1aw, mustRoomPkt(t, envelope.OpLeave, t0, maine)); err != nil {
		t.Fatal(err)
	}
	if got := s.Expire(t0 + 100); len(got) != 0 {
		t.Errorf("expired at the boundary: %v", got)
	}
	// Hearing N1ADJ refreshes it; W1AW's opt-out ages out.
	if err := s.Heard(n1adj, t0+50); err != nil {
		t.Fatal(err)
	}
	if got := s.Expire(t0 + 101); !reflect.DeepEqual(got, []string{"W1AW"}) {
		t.Errorf("Expire = %v, want [W1AW]", got)
	}
	if got := s.Expire(t0 + 151); !reflect.DeepEqual(got, []string{"N1ADJ"}) {
		t.Errorf("Expire = %v, want [N1ADJ]", got)
	}
	if got := s.ActiveRooms(); len(got) != 0 {
		t.Errorf("ActiveRooms after expiry = %v", got)
	}
	if s.expiry != 100 {
		t.Errorf("expiry = %d", s.expiry)
	}
	if d := newSubs(t, SubscriptionsConfig{}); d.expiry != DefaultSubscriptionExpiry {
		t.Errorf("default expiry = %d", d.expiry)
	}
}

func TestParseRoomCommand(t *testing.T) {
	maine, dev := mustRoom(t, "MAINE"), mustRoom(t, "M17DEV")
	tests := []struct {
		text  string
		op    envelope.RoomOp
		rooms []envelope.Address
		err   error
	}{
		{"/join MAINE", envelope.OpJoin, []envelope.Address{maine}, nil},
		{"/JOIN maine m17dev", envelope.OpJoin, []envelope.Address{maine, dev}, nil},
		{"  /leave MAINE  ", envelope.OpLeave, []envelope.Address{maine}, nil},
		{"/rooms", envelope.OpList, nil, nil},
		{"/Rooms\n", envelope.OpList, nil, nil},
		{"/join #MAINE", 0, nil, ErrBadRoomName},
		{"/join MAINELOBSTER", 0, nil, ErrBadRoomName},
		{"/join MA.INE", 0, nil, ErrBadRoomName},
		{"/join", 0, nil, ErrBadRoomName},
		{"/rooms MAINE", 0, nil, ErrUnknownCmd},
		{"/part MAINE", 0, nil, ErrUnknownCmd},
		{"/", 0, nil, ErrUnknownCmd},
		{"hello everyone", 0, nil, ErrNotCommand},
		{"", 0, nil, ErrNotCommand},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			op, rooms, err := ParseRoomCommand(tt.text)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if err != nil {
				return
			}
			if op != tt.op || !sameAddrs(rooms, tt.rooms) {
				t.Errorf("= %s %v, want %s %v", op, rooms, tt.op, tt.rooms)
			}
		})
	}
}

func sameAddrs(a, b []envelope.Address) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	seen := map[envelope.Address]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
