package qtcd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
	"github.com/libp2p/go-libp2p/core/peer"
)

// homed is the state for one base callsign this node homes (node protocol §7).
type homed struct {
	base      envelope.Address
	lastSweep uint32
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
// not already homed: WATCH on every mailbox member, then a sweep.
func (r *Station) Heard(device envelope.Address, via Via, now uint32) error {
	if !device.IsStandard() {
		return fmt.Errorf("%w: %s", ErrNotCallsign, device)
	}
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
	r.mu.Unlock()
	if fresh {
		r.log.Info("homing", "callsign", base, "device", device, "via", via)
		r.go_(func() { r.home(h) })
	}
	return nil
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
	now := unixNow()
	since := h.lastSweep
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
			union[id2] = e
		}
	}
	var local map[envelope.ID]bool
	if r.mem != nil {
		// This station's own mailbox is a member too.
		recs, _, _, err := r.mem.Query(h.base, since, store.MaxLimit, []envelope.PacketType{envelope.TypeMSG, envelope.TypeRCPT, envelope.TypeROOM})
		if err == nil {
			reached++
			local = map[envelope.ID]bool{}
			for _, rec := range recs {
				local[rec.ID()] = true
				union[rec.ID()] = rec.Env
			}
		}
	}
	if reached == 0 {
		return false
	}
	if len(failed) > 0 && rec.HomeStation == r.host.ID() {
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

	// Which messages already have a DELIVERED receipt in the mailbox?
	delivered := map[envelope.ID]bool{}
	for _, e := range union {
		if rc, ok := e.Rcpt(); ok && rc.Status() == envelope.StatusDelivered {
			delivered[rc.MessageID()] = true
		}
	}
	window := uint32(r.cfg.ReplayWindow / time.Second)
	for _, e := range union {
		switch e.Type() {
		case envelope.TypeROOM:
			if err := r.subs.Apply(h.base, e); err != nil {
				r.log.Debug("stored ROOM envelope not applied", "callsign", h.base, "err", err)
			}
		case envelope.TypeMSG:
			if delivered[e.ID()] {
				continue
			}
			ts := e.Timestamp()
			if ts != 0 && now-ts > window {
				continue
			}
			r.deliverLocal(e)
		case envelope.TypeRCPT:
			r.deliverLocal(e)
		}
	}
	h.lastSweep = now
	r.log.Debug("sweep done", "callsign", h.base, "members_reached", reached, "envelopes", len(union))
	return true
}

// onStored handles an EVENT from a mailbox member: a new envelope stored for
// a callsign this node watches.
func (r *Station) onStored(callsign envelope.Address, e *envelope.Envelope, from peer.ID) {
	r.log.Debug("event", "callsign", callsign, "member", from, "envelope", e)
	switch e.Type() {
	case envelope.TypeROOM:
		if err := r.subs.Apply(callsign, e); err != nil {
			r.log.Debug("ROOM event not applied", "callsign", callsign, "err", err)
		}
	default:
		r.deliverLocal(e)
	}
}

// deliverLocal delivers a MSG or RCPT to the local devices it addresses,
// once per message ID and device (node protocol §7.4), issuing TRANSMITTED
// when a MSG asked for a receipt.
func (r *Station) deliverLocal(e *envelope.Envelope) {
	dst := e.Destination()
	var devices []envelope.Address
	switch {
	case dst.IsRoom():
		for _, base := range r.subs.Subscribers(dst) {
			a, err := envelope.EncodeAddress(base)
			if err != nil {
				continue
			}
			devices = append(devices, r.presence.localDevices(a)...)
		}
	case dst.IsStandard():
		if dst.Base() == dst {
			devices = r.presence.localDevices(dst)
		} else if r.presence.lastHeard(dst) != 0 {
			devices = []envelope.Address{dst}
		}
	}
	now := unixNow()
	for _, d := range devices {
		if dst.IsRoom() && d == e.Source() && !r.cfg.EchoRoomMessages {
			continue
		}
		if !r.markDelivered(e, d) {
			continue
		}
		r.deliverTo(d, e)
		if m, ok := e.Msg(); ok && m.RcptReq() && !dst.IsRoom() {
			r.issueReceipt(e, envelope.StatusTransmitted, r.presence.lastHeard(d), now)
		}
	}
}

// markDelivered records a (message, device) delivery, returning false if it
// already happened. RCPT and ROOM use their StoreID.
func (r *Station) markDelivered(e *envelope.Envelope, device envelope.Address) bool {
	k := deliveryKey{id: e.StoreID(), device: device}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, done := r.delivered[k]; done {
		return false
	}
	r.delivered[k] = struct{}{}
	return true
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
	switch e.Type() {
	case envelope.TypeMSG:
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
	case envelope.TypeRCPT:
		r.mailbox.putAll(e.Destination().Base(), e, nil)
		return nil
	}
	return fmt.Errorf("qtcd: Send does not accept %s", e.Type())
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
		recs, _, _, err := r.mem.Query(base, 0, store.MaxLimit, []envelope.PacketType{envelope.TypeMSG, envelope.TypeRCPT, envelope.TypeROOM})
		if err == nil {
			for _, rec := range recs {
				union[rec.ID()] = rec.Env
			}
		}
	}
	return union
}
