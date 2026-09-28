package qtcd

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
	"github.com/libp2p/go-libp2p/core/peer"
)

// homed is the state for one base callsign this node homes (node protocol §7).
type homed struct {
	base      envelope.Address
	sweepMu   sync.Mutex // one sweep at a time, so replays never overlap
	lastSweep uint32     // guarded by sweepMu
	mu        sync.Mutex
	watched   map[peer.ID]bool // members this node has WATCHed
	failures  map[peer.ID]int  // consecutive sweeps a member was unreachable
}

// homes reports whether this node currently homes a base callsign.
func (r *Station) homes(base envelope.Address) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.homed[base.Base()]
	return ok
}

// Heard tells the station that a local device was heard, via RF, a local
// client, or an internet client. It publishes presence, auto-subscribes the
// callsign to the local room, and begins homing the base callsign if it is
// not already homed: WATCH on every mailbox member, then a sweep. A device
// of a callsign already homed that is heard again after being out of reach,
// or that has messages held for it, gets a full sweep and replay.
func (r *Station) Heard(device envelope.Address, via Via, now uint32) error {
	if !device.IsStandard() {
		return fmt.Errorf("%w: %s", ErrNotCallsign, device)
	}
	wasReachable := r.reachable(device, now)
	r.presence.heard(device, via, now)
	if err := r.subs.Heard(device, now); err != nil {
		return err
	}
	base := device.Base()
	r.mu.Lock()
	h := r.homed[base]
	fresh := h == nil
	if fresh {
		h = &homed{base: base, watched: map[peer.ID]bool{}, failures: map[peer.ID]int{}}
		r.homed[base] = h
	}
	held := r.held[device]
	delete(r.held, device)
	r.mu.Unlock()
	switch {
	case fresh:
		r.log.Info("homing", "callsign", base, "device", device, "via", via)
		r.go_(func() { r.home(h) })
	case held || !wasReachable:
		r.log.Info("device back in reach; replaying", "device", device, "held", held)
		r.go_(func() { r.sweepSince(h, 0) })
	}
	return nil
}

// Relinked tells the station that devices' link to this node is back (a
// gateway that restarted and relinked). Any with messages held for them get
// a replay now, rather than when they are next heard.
func (r *Station) Relinked(devices []envelope.Address) {
	for _, d := range devices {
		r.mu.Lock()
		held := r.held[d]
		delete(r.held, d)
		h := r.homed[d.Base()]
		r.mu.Unlock()
		if held && h != nil {
			r.log.Info("link back; replaying held messages", "device", d)
			r.go_(func() { r.sweepSince(h, 0) })
		}
	}
}

// Homed reports the base callsigns this node currently homes.
func (r *Station) Homed() []envelope.Address {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]envelope.Address, 0, len(r.homed))
	for a := range r.homed {
		out = append(out, a)
	}
	return out
}

// home runs the homing loop for one base callsign: watch, sweep, and
// resweep at the configured interval while the station runs.
func (r *Station) home(h *homed) {
	// Until a first watch and sweep succeed (the mailbox node may not be
	// reachable yet), retry every 30 seconds rather than at the sweep
	// interval.
	for {
		watched := r.watchMembers(h, r.mailbox.membersFor(h.base, true))
		if r.server != nil {
			watched++ // our own mailbox reports stores through OnStored
		}
		if watched > 0 && r.sweep(h) {
			break
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	t := time.NewTicker(r.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.sweep(h)
		}
	}
}

// watchMembers WATCHes any member not yet watched for h and returns how
// many members are watched in total.
func (r *Station) watchMembers(h *homed, members []peer.ID) int {
	for _, id := range members {
		if id == r.host.ID() {
			continue
		}
		h.mu.Lock()
		done := h.watched[id]
		h.mu.Unlock()
		if done {
			continue
		}
		ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
		err := r.mailbox.conn(id).watch(ctx, h.base)
		cancel()
		if err != nil {
			r.log.Warn("watch failed", "member", id, "callsign", h.base, "err", err)
			continue
		}
		h.mu.Lock()
		h.watched[id] = true
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watched)
}

// sweep queries every member since the last sweep, unions by StoreID, puts
// to each member whatever it lacks, applies ROOM state, and replays recent
// undelivered messages to local devices (node protocol §7.2, §7.1 step 5).
// It reports whether any member was reached.
func (r *Station) sweep(h *homed) bool {
	return r.sweepSince(h, -1)
}

// sweepSince sweeps from a given time; -1 means since the last sweep, 0
// means everything the mailbox holds.
func (r *Station) sweepSince(h *homed, from int64) bool {
	h.sweepMu.Lock()
	defer h.sweepMu.Unlock()
	now := unixNow()
	since := h.lastSweep
	if from >= 0 {
		since = uint32(from)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()
	rec, err := r.records.resolve(ctx, h.base, true)
	if err != nil {
		r.log.Warn("sweep: no mailbox record", "callsign", h.base, "err", err)
		return false
	}
	members := rec.Members
	r.watchMembers(h, members)

	union := map[envelope.ID]*envelope.Envelope{}
	// order is the union in the order first seen. A mailbox returns records
	// in the order it received them, which is the only ordering for messages
	// sent within one second (envelope timestamps are whole seconds).
	var order []envelope.ID
	add := func(id envelope.ID, e *envelope.Envelope) {
		if _, ok := union[id]; !ok {
			order = append(order, id)
		}
		union[id] = e
	}
	have := map[peer.ID]map[envelope.ID]bool{}
	reached := 0
	var failed []peer.ID
	for _, id := range members {
		if id == r.host.ID() {
			continue // our own store is read below
		}
		envs, err := r.mailbox.query(ctx, id, h.base, since)
		if err != nil {
			r.log.Warn("sweep query failed", "member", id, "callsign", h.base, "err", err)
			h.mu.Lock()
			h.failures[id]++
			if h.failures[id] >= r.cfg.MemberFailSweeps {
				failed = append(failed, id)
			}
			h.mu.Unlock()
			continue
		}
		h.mu.Lock()
		h.failures[id] = 0
		h.mu.Unlock()
		reached++
		have[id] = map[envelope.ID]bool{}
		for _, e := range envs {
			id2 := e.StoreID()
			have[id][id2] = true
			add(id2, e)
		}
	}
	var local map[envelope.ID]bool
	if r.mem != nil {
		// This station's own mailbox is a member too.
		recs, _, _, err := r.mem.Query(h.base, since, store.MaxLimit, []envelope.Kind{envelope.KindMSG, envelope.KindRCPT, envelope.KindROOM})
		if err == nil {
			reached++
			local = map[envelope.ID]bool{}
			for _, rec := range recs {
				local[rec.ID()] = true
				add(rec.ID(), rec.Env)
			}
		}
	}
	if reached == 0 {
		return false
	}
	if (len(failed) > 0 || len(rec.Members) < int(rec.K)) && rec.HomeStation == r.host.ID() {
		// Repair copies the callsign's whole retained history to the
		// recruit, not just this sweep's increment.
		if next, err := r.records.repair(ctx, rec, failed, r.fullHistory(ctx, h.base, members)); err != nil {
			r.log.Warn("mailbox repair failed", "callsign", h.base, "err", err)
		} else if next != nil {
			h.mu.Lock()
			for _, id := range failed {
				delete(h.failures, id)
				delete(h.watched, id)
			}
			h.mu.Unlock()
			r.watchMembers(h, next.Members)
		}
	}
	for id, ids := range have {
		for sid, e := range union {
			if !ids[sid] {
				r.go_(func() { r.mailbox.putRetry(id, h.base, e, nil, nil) })
			}
		}
	}
	if local != nil {
		for sid, e := range union {
			if !local[sid] {
				if _, err := r.server.PutLocal(h.base, e); err != nil {
					r.log.Debug("local mailbox refused sweep envelope", "id", sid, "err", err)
				}
			}
		}
	}

	// Which messages are done: a DELIVERED receipt, or a delivery record from
	// any node (node protocol §7.4) saying it was transmitted or skipped?
	delivered := map[envelope.ID]bool{}
	for _, e := range union {
		if rc, ok := e.Rcpt(); ok && (rc.Status() == envelope.StatusDelivered || isDeliveryRecord(e)) {
			delivered[rc.MessageID()] = true
		}
	}
	var replay []*envelope.Envelope
	for _, sid := range order {
		e := union[sid]
		switch e.Kind() {
		case envelope.KindROOM:
			if err := r.subs.Apply(h.base, e); err != nil {
				r.log.Debug("stored ROOM envelope not applied", "callsign", h.base, "err", err)
			}
		case envelope.KindMSG:
			if !delivered[e.ID()] {
				replay = append(replay, e)
			}
		case envelope.KindRCPT:
			r.deliverLocal(e)
		}
	}
	r.replay(h.base, replay, now)
	if now > h.lastSweep {
		h.lastSweep = now
	}
	r.log.Debug("sweep done", "callsign", h.base, "members_reached", reached, "envelopes", len(union))
	return true
}

// onStored handles an EVENT from a mailbox member: a new envelope stored for
// a callsign this node watches.
func (r *Station) onStored(callsign envelope.Address, e *envelope.Envelope, from peer.ID) {
	r.log.Debug("event", "callsign", callsign, "member", from, "envelope", e)
	switch e.Kind() {
	case envelope.KindROOM:
		if err := r.subs.Apply(callsign, e); err != nil {
			r.log.Debug("ROOM event not applied", "callsign", callsign, "err", err)
		}
	default:
		r.deliverLocal(e)
	}
}

// Delivery to local devices (node protocol §7.4). A device is reachable
// while it was heard within ReachWindow. A message for a device that is out
// of reach, or whose gateway link is down, is held: it stays undelivered in
// the mailbox, and the device gets a replay when it is next heard.

// deliverLocal delivers a MSG or RCPT to the local devices it addresses.
// Delivery records are for nodes, not devices.
func (r *Station) deliverLocal(e *envelope.Envelope) {
	if isDeliveryRecord(e) {
		return
	}
	now := unixNow()
	for _, d := range r.targets(e) {
		r.deliverOne(e, d, now)
	}
}

// targets returns the local devices e addresses: every device of a base
// callsign, the one device a suffixed callsign names, or every device of a
// room's subscribers, less the sender's own device for a room message
// unless EchoRoomMessages is set.
func (r *Station) targets(e *envelope.Envelope) []envelope.Address {
	dst := e.Destination()
	var devices []envelope.Address
	switch {
	case dst.IsRoom():
		for _, base := range r.subs.Subscribers(dst) {
			a, err := envelope.EncodeAddress(base)
			if err != nil {
				continue
			}
			for _, d := range r.presence.localDevices(a) {
				if d != e.Source() || r.cfg.EchoRoomMessages {
					devices = append(devices, d)
				}
			}
		}
	case dst.IsStandard():
		if dst.Base() == dst {
			devices = r.presence.localDevices(dst)
		} else if r.presence.lastHeard(dst) != 0 {
			devices = []envelope.Address{dst}
		}
	}
	return devices
}

// deliverOne hands e to device d once (node protocol §7.4), issuing
// TRANSMITTED when a MSG asked for a receipt, and reports whether it did. A
// MSG for a device out of reach or without a working link is held.
func (r *Station) deliverOne(e *envelope.Envelope, d envelope.Address, now uint32) bool {
	k := deliveryKey{id: e.StoreID(), device: d}
	if !r.reachable(d, now) {
		if e.Kind() == envelope.KindMSG && !r.delivered.has(k) {
			r.hold(d, e)
		}
		return false
	}
	if !r.delivered.mark(k, now) {
		return false
	}
	switch r.deliverTo(d, e) {
	case deliverNone:
		// Marked first so concurrent paths cannot both send; undone
		// because nothing was sent.
		r.delivered.forget(k)
		if e.Kind() == envelope.KindMSG {
			r.hold(d, e)
		}
		return false
	case deliverPending:
		// A native device: recorded when it acknowledges (inetAcked), or
		// held if it never does (inetLost).
		return true
	}
	if m, ok := e.Msg(); ok {
		r.recordDelivery(e, d, envelope.StatusTransmitted, recordTransmitted, now)
		if m.RcptReq() && !e.Destination().IsRoom() {
			r.issueReceipt(e, envelope.StatusTransmitted, r.presence.lastHeard(d), now)
		}
	}
	return true
}

// Delivery records (node protocol §7.4) are RCPTs a node stores in a
// recipient's own mailbox, addressed to the recipient, saying a MSG was
// transmitted to one of its devices or left out by the replay limit. Other
// nodes homing the callsign read them in their sweeps and do not replay
// those messages, so a radio moving between hotspots is not sent the same
// messages again. The note marks them; they are never delivered to a device
// and are not receipts to the sender, whose receipts are unchanged.
const (
	recordTransmitted = "qtc:delivered"
	recordSkipped     = "qtc:replay-limit"
)

func isDeliveryRecord(e *envelope.Envelope) bool {
	rc, ok := e.Rcpt()
	if !ok {
		return false
	}
	switch rc.Note() {
	case recordTransmitted, recordSkipped:
		return true
	}
	return false
}

// recordDelivery stores a delivery record for msg in the mailbox of device's
// callsign.
func (r *Station) recordDelivery(msg *envelope.Envelope, device envelope.Address, status envelope.Status, note string, now uint32) {
	base := device.Base()
	rc, err := envelope.BuildRcpt(r.callsign, base, msg.ID(), status, now, 0, note)
	if err != nil {
		r.log.Error("build delivery record", "err", err)
		return
	}
	r.mailbox.putAll(base, rc, nil)
}

// reachable reports whether device was heard within ReachWindow.
func (r *Station) reachable(device envelope.Address, now uint32) bool {
	last := r.presence.lastHeard(device)
	return last != 0 && uint64(last)+uint64(r.cfg.ReachWindow/time.Second) >= uint64(now)
}

// hold notes that a message is waiting for device, so the next time the
// device is heard it gets a replay.
func (r *Station) hold(d envelope.Address, e *envelope.Envelope) {
	r.mu.Lock()
	first := !r.held[d]
	r.held[d] = true
	r.mu.Unlock()
	if first {
		r.log.Info("holding messages for device", "device", d, "reachable", r.reachable(d, unixNow()))
	}
	r.log.Debug("held for device", "device", d, "envelope", e)
}

// replay delivers the messages a sweep of base's mailbox found without a
// DELIVERED receipt (node protocol §7.1): to each reachable local device of
// base, the ReplayLimit
// most recent it has not had, oldest first. msgs is in mailbox order, which
// breaks ties between messages with the same timestamp. Older ones are recorded as
// skipped so they never come back, and the device gets one notice saying
// how many.
func (r *Station) replay(base envelope.Address, msgs []*envelope.Envelope, now uint32) {
	pending := map[envelope.Address][]*envelope.Envelope{}
	for _, e := range msgs {
		for _, d := range r.targets(e) {
			// Only base's own devices: base's mailbox also holds copies of
			// messages base sent, and their recipients' delivery records
			// are in the recipients' mailboxes, not this one. A recipient
			// gets its replay from its own mailbox's sweep.
			if d.Base() != base {
				continue
			}
			if !r.delivered.has(deliveryKey{id: e.StoreID(), device: d}) {
				pending[d] = append(pending[d], e)
			}
		}
	}
	for d, list := range pending {
		if !r.reachable(d, now) {
			r.hold(d, list[0])
			continue
		}
		if r.inet != nil && r.inet.isNative(d) {
			// A native device fetches its backlog with SYNC when it
			// chooses; it is told how much is waiting (client spec §5.3).
			if !r.inet.notify(d, len(list)) {
				r.hold(d, list[0])
			}
			continue
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Timestamp() < list[j].Timestamp() })
		skipped := 0
		if n := len(list) - r.cfg.ReplayLimit; n > 0 {
			for _, e := range list[:n] {
				if r.delivered.skip(deliveryKey{id: e.StoreID(), device: d}, now) {
					r.recordDelivery(e, d, envelope.StatusExpired, recordSkipped, now)
					skipped++
				}
			}
			list = list[n:]
		}
		if skipped > 0 {
			r.notice(d, skipped, now)
		}
		sent := 0
		for _, e := range list {
			if r.deliverOne(e, d, now) {
				sent++
			}
		}
		r.log.Info("replayed", "device", d, "sent", sent, "skipped", skipped)
	}
}

// notice tells a device, in a message from the node, how many older
// messages the replay cap left out. It is not stored or journaled.
func (r *Station) notice(d envelope.Address, skipped int, now uint32) {
	text := fmt.Sprintf("%d older messages not sent", skipped)
	if skipped == 1 {
		text = "1 older message not sent"
	}
	e, err := envelope.BuildMsg(r.callsign, d, now, r.cfg.DefaultTTL, uint16(now), 0, text)
	if err != nil {
		r.log.Warn("build replay notice", "err", err)
		return
	}
	r.deliverTo(d, e)
}

// markDelivered records a (message, device) delivery, returning false if it
// already happened. RCPT and ROOM use their StoreID.
func (r *Station) markDelivered(e *envelope.Envelope, device envelope.Address) bool {
	return r.delivered.mark(deliveryKey{id: e.StoreID(), device: device}, unixNow())
}

// issueReceipt builds a receipt from this node about msg and stores it on
// the original sender's mailbox.
func (r *Station) issueReceipt(msg *envelope.Envelope, status envelope.Status, lastHeard, now uint32) {
	rc, err := envelope.BuildRcpt(r.callsign, msg.Source(), msg.ID(), status, now, lastHeard, "")
	if err != nil {
		r.log.Error("build receipt", "err", err)
		return
	}
	r.log.Debug("issuing receipt", "receipt", rc)
	r.mailbox.putAll(msg.Source().Base(), rc, nil)
}

// Send accepts an envelope from a local device (node protocol §6). A MSG to
// a callsign is stored on the destination's and the sender's mailboxes, with
// QUEUED issued once if requested. A MSG to a room is published to the room
// topic, stored on each local subscriber's mailbox, and records an implicit
// join. A RCPT goes to its destination's mailbox. ROOM requests are answered
// with HandleRoom, not Send.
func (r *Station) Send(e *envelope.Envelope) error {
	switch e.Kind() {
	case envelope.KindMSG:
		m, _ := e.Msg()
		dst := e.Destination()
		switch {
		case dst.IsRoom():
			if !r.subs.Carries(dst) {
				return fmt.Errorf("%w: %s", ErrNotValidRoom, dst)
			}
			join, err := r.subs.ImplicitJoin(e.Source(), dst, e.Timestamp(), unixNow())
			if err != nil {
				return err
			}
			r.mailbox.putAll(e.Source().Base(), join, nil)
			r.rooms.publish(dst, e)
			r.storeRoomMessage(e)
			r.deliverLocal(e)
			return nil
		case dst.IsStandard():
			var onFirst func()
			if m.RcptReq() {
				onFirst = func() {
					r.mu.Lock()
					_, done := r.queued[e.ID()]
					r.queued[e.ID()] = struct{}{}
					r.mu.Unlock()
					if !done {
						r.issueReceipt(e, envelope.StatusQueued, 0, unixNow())
					}
				}
			}
			r.mailbox.putAll(dst.Base(), e, onFirst)
			if dst.Base() != e.Source().Base() {
				r.mailbox.putAll(e.Source().Base(), e, nil)
			}
			// Fast path for a destination homed here; the EVENT would also
			// deliver, and dedup makes that harmless.
			r.deliverLocal(e)
			return nil
		}
		return fmt.Errorf("qtcd: cannot send to %s", dst)
	case envelope.KindRCPT:
		r.mailbox.putAll(e.Destination().Base(), e, nil)
		return nil
	}
	return fmt.Errorf("qtcd: Send does not accept %s", e.Kind())
}

// HandleRoom processes a ROOM request from a local device and returns the
// reply. Accepted JOIN and LEAVE state is stored on the callsign's mailbox.
func (r *Station) HandleRoom(device envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error) {
	reply, toStore, err := r.subs.Handle(device, req, unixNow())
	if err != nil {
		return nil, err
	}
	if toStore != nil {
		r.mailbox.putAll(device.Base(), toStore, nil)
	}
	return reply, nil
}

// storeRoomMessage queues a room MSG on the mailbox of every local subscriber
// (rooms spec §7.2).
func (r *Station) storeRoomMessage(e *envelope.Envelope) {
	for _, base := range r.subs.Subscribers(e.Destination()) {
		a, err := envelope.EncodeAddress(base)
		if err != nil {
			continue
		}
		if len(r.presence.localDevices(a)) == 0 {
			continue
		}
		r.mailbox.putAll(a, e, nil)
	}
}

// fullHistory unions everything the reachable members and this node's own
// store hold for a base callsign.
func (r *Station) fullHistory(ctx context.Context, base envelope.Address, members []peer.ID) map[envelope.ID]*envelope.Envelope {
	union := map[envelope.ID]*envelope.Envelope{}
	for _, id := range members {
		if id == r.host.ID() {
			continue
		}
		envs, err := r.mailbox.query(ctx, id, base, 0)
		if err != nil {
			continue
		}
		for _, e := range envs {
			union[e.StoreID()] = e
		}
	}
	if r.mem != nil {
		recs, _, _, err := r.mem.Query(base, 0, store.MaxLimit, []envelope.Kind{envelope.KindMSG, envelope.KindRCPT, envelope.KindROOM})
		if err == nil {
			for _, rec := range recs {
				union[rec.ID()] = rec.Env
			}
		}
	}
	return union
}
