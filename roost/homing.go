package roost

import (
	"context"
	"fmt"
	"time"

	"github.com/jancona/pigeon/envelope"
	"github.com/libp2p/go-libp2p/core/peer"
)

// homed is the state for one base callsign this node homes (node protocol §7).
type homed struct {
	base      envelope.Address
	lastSweep uint32
}

// Heard tells the roost that a local device was heard, via RF, a local
// client, or an internet client. It publishes presence, auto-subscribes the
// callsign to the local room, and begins homing the base callsign if it is
// not already homed: WATCH on every inbox member, then a sweep.
func (r *Roost) Heard(device envelope.Address, via Via, now uint32) error {
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
		h = &homed{base: base}
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
func (r *Roost) Homed() []envelope.Address {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]envelope.Address, 0, len(r.homed))
	for a := range r.homed {
		out = append(out, a)
	}
	return out
}

// home runs the homing loop for one base callsign: watch, sweep, and
// resweep at the configured interval while the roost runs.
func (r *Roost) home(h *homed) {
	// Until a first watch and sweep succeed (the inbox node may not be
	// reachable yet), retry every 30 seconds rather than at the sweep
	// interval.
	for {
		watched := 0
		for _, id := range r.inbox.membersFor(h.base) {
			ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
			err := r.inbox.conn(id).watch(ctx, h.base)
			cancel()
			if err != nil {
				r.log.Warn("watch failed", "member", id, "callsign", h.base, "err", err)
				continue
			}
			watched++
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

// sweep queries every member since the last sweep, unions by StoreID, puts
// to each member whatever it lacks, applies ROOM state, and replays recent
// undelivered messages to local devices (node protocol §7.2, §7.1 step 5).
// It reports whether any member was reached.
func (r *Roost) sweep(h *homed) bool {
	now := unixNow()
	since := h.lastSweep
	members := r.inbox.membersFor(h.base)
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()

	union := map[envelope.ID]*envelope.Envelope{}
	have := map[peer.ID]map[envelope.ID]bool{}
	reached := 0
	for _, id := range members {
		envs, err := r.inbox.query(ctx, id, h.base, since)
		if err != nil {
			r.log.Warn("sweep query failed", "member", id, "callsign", h.base, "err", err)
			continue
		}
		reached++
		have[id] = map[envelope.ID]bool{}
		for _, e := range envs {
			id2 := e.StoreID()
			have[id][id2] = true
			union[id2] = e
		}
	}
	if reached == 0 {
		return false
	}
	for id, ids := range have {
		for sid, e := range union {
			if !ids[sid] {
				r.go_(func() { r.inbox.putRetry(id, h.base, e, nil, nil) })
			}
		}
	}

	// Which messages already have a DELIVERED receipt in the inbox?
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

// onStored handles an EVENT from an inbox member: a new envelope stored for
// a callsign this node watches.
func (r *Roost) onStored(callsign envelope.Address, e *envelope.Envelope, from peer.ID) {
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
func (r *Roost) deliverLocal(e *envelope.Envelope) {
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
		if !r.markDelivered(e, d) {
			continue
		}
		r.cfg.Deliver(d, e)
		if m, ok := e.Msg(); ok && m.RcptReq() && !dst.IsRoom() {
			r.issueReceipt(e, envelope.StatusTransmitted, r.presence.lastHeard(d), now)
		}
	}
}

// markDelivered records a (message, device) delivery, returning false if it
// already happened. RCPT and ROOM use their StoreID.
func (r *Roost) markDelivered(e *envelope.Envelope, device envelope.Address) bool {
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
// the original sender's inbox.
func (r *Roost) issueReceipt(msg *envelope.Envelope, status envelope.Status, lastHeard, now uint32) {
	rc, err := envelope.BuildRcpt(r.callsign, msg.Source(), msg.ID(), status, now, lastHeard, "")
	if err != nil {
		r.log.Error("build receipt", "err", err)
		return
	}
	r.log.Debug("issuing receipt", "receipt", rc)
	r.inbox.putAll(msg.Source().Base(), rc, nil)
}

// Send accepts an envelope from a local device (node protocol §6). A MSG to
// a callsign is stored on the destination's and the sender's inboxes, with
// QUEUED issued once if requested. A MSG to a room is published to the room
// topic, stored on each local subscriber's inbox, and records an implicit
// join. A RCPT goes to its destination's inbox. ROOM requests are answered
// with HandleRoom, not Send.
func (r *Roost) Send(e *envelope.Envelope) error {
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
			r.inbox.putAll(e.Source().Base(), join, nil)
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
			r.inbox.putAll(dst.Base(), e, onFirst)
			if dst.Base() != e.Source().Base() {
				r.inbox.putAll(e.Source().Base(), e, nil)
			}
			// Fast path for a destination homed here; the EVENT would also
			// deliver, and dedup makes that harmless.
			r.deliverLocal(e)
			return nil
		}
		return fmt.Errorf("roost: cannot send to %s", dst)
	case envelope.TypeRCPT:
		r.inbox.putAll(e.Destination().Base(), e, nil)
		return nil
	}
	return fmt.Errorf("roost: Send does not accept %s", e.Type())
}

// HandleRoom processes a ROOM request from a local device and returns the
// reply. Accepted JOIN and LEAVE state is stored on the callsign's inbox.
func (r *Roost) HandleRoom(device envelope.Address, req *envelope.Envelope) (*envelope.Envelope, error) {
	reply, toStore, err := r.subs.Handle(device, req, unixNow())
	if err != nil {
		return nil, err
	}
	if toStore != nil {
		r.inbox.putAll(device.Base(), toStore, nil)
	}
	return reply, nil
}

// storeRoomMessage queues a room MSG on the inbox of every local subscriber
// (rooms spec §7.2).
func (r *Roost) storeRoomMessage(e *envelope.Envelope) {
	for _, base := range r.subs.Subscribers(e.Destination()) {
		a, err := envelope.EncodeAddress(base)
		if err != nil {
			continue
		}
		if len(r.presence.localDevices(a)) == 0 {
			continue
		}
		r.inbox.putAll(a, e, nil)
	}
}
