package qtcd

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
)

// The station's side of native devices (docs/qtc-client.md): what happens
// when one acknowledges a message or never does, and the backlog a SYNC
// REQUEST pages through.

// inetAcked records a native device's acknowledgement of msg (client spec
// §4.2). fresh means a live delivery, already marked delivered when sent; a
// sync page's message is marked now, and recorded only if it is new to the
// device. A DELIVERED goes on to the sender if it asked for a receipt.
func (r *Station) inetAcked(device envelope.Address, msg, rcpt *envelope.Envelope, fresh bool) {
	now := unixNow()
	if !fresh && !r.delivered.mark(deliveryKey{id: msg.StoreID(), device: device}, now) {
		return
	}
	r.recordDelivery(msg, device, envelope.StatusDelivered, recordTransmitted, now)
	if m, ok := msg.Msg(); ok && rcpt != nil && m.RcptReq() && !msg.Destination().IsRoom() {
		if err := r.Send(rcpt); err != nil {
			r.log.Warn("DELIVERED not passed on", "receipt", rcpt, "err", err)
		}
	}
}

// inetLost handles a native device that never acknowledged msg: a live
// delivery is undone and held for when the device is next heard. A sync
// page's message is simply offered again by the next sync.
func (r *Station) inetLost(device envelope.Address, msg *envelope.Envelope, fresh bool) {
	if !fresh {
		return
	}
	r.delivered.forget(deliveryKey{id: msg.StoreID(), device: device})
	r.hold(device, msg)
}

func (r *Station) inetReachable(device envelope.Address) bool {
	return r.reachable(device, unixNow())
}

func (r *Station) inetSubscribed(device, room envelope.Address) bool {
	rooms, err := r.subs.Rooms(device)
	return err == nil && slices.Contains(rooms, room)
}

// errMailboxUnreachable refuses a sync when no mailbox member answered.
var errMailboxUnreachable = errors.New("mailbox unreachable; try again later")

// syncEntry is one envelope of a callsign's mailbox, positioned by the
// earliest received-at time any member reports (client spec §5.2).
type syncEntry struct {
	rec  store.Record
	seen int // order first seen, to keep mailbox order within a second
}

// inetSync pages through device's mailbox from the request's Cursor and
// Skip (client spec §5.1). The position counts every entry the device could
// be sent, acknowledged or not, so it does not move when the device
// acknowledges what it was sent; entries the device already acknowledged
// are passed over unless the request says ALL.
func (r *Station) inetSync(device envelope.Address, req envelope.Sync, limit int) ([]*envelope.Envelope, uint32, uint16, int, error) {
	base := device.Base()
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	cursor, skip := req.Cursor(), int(req.Skip())
	entries := map[envelope.ID]*syncEntry{}
	add := func(rec store.Record) {
		id := rec.ID()
		if e, ok := entries[id]; ok {
			if rec.ReceivedAt < e.rec.ReceivedAt {
				e.rec.ReceivedAt = rec.ReceivedAt
			}
			return
		}
		entries[id] = &syncEntry{rec: rec, seen: len(entries)}
	}
	reached := 0
	if rec, err := r.records.resolve(ctx, base, true); err == nil {
		for _, id := range rec.Members {
			if id == r.host.ID() {
				continue
			}
			recs, err := r.mailbox.queryRecords(ctx, id, base, cursor)
			if err != nil {
				r.log.Debug("sync query failed", "member", id, "callsign", base, "err", err)
				continue
			}
			reached++
			for _, rc := range recs {
				add(rc)
			}
		}
	}
	if r.mem != nil {
		if recs, _, _, err := r.mem.Query(base, cursor, store.MaxLimit, []envelope.Kind{envelope.KindMSG, envelope.KindRCPT}); err == nil {
			reached++
			for _, rc := range recs {
				add(rc)
			}
		}
	}
	if reached == 0 {
		return nil, 0, 0, 0, errMailboxUnreachable
	}

	var list []*syncEntry
	for _, e := range entries {
		if e.rec.ReceivedAt >= cursor && syncWants(device, req, e.rec.Env) {
			list = append(list, e)
		}
	}
	slices.SortFunc(list, func(a, b *syncEntry) int {
		if a.rec.ReceivedAt != b.rec.ReceivedAt {
			return int(int64(a.rec.ReceivedAt) - int64(b.rec.ReceivedAt))
		}
		return a.seen - b.seen
	})

	var page []*envelope.Envelope
	pos := 0
	for pos < len(list) && list[pos].rec.ReceivedAt == cursor && skip > 0 {
		pos++
		skip--
	}
	nextCursor, nextSkip := cursor, int(req.Skip())
	for ; pos < len(list) && len(page) < limit; pos++ {
		e := list[pos]
		if e.rec.ReceivedAt != nextCursor {
			nextCursor, nextSkip = e.rec.ReceivedAt, 0
		}
		nextSkip++
		if r.acknowledged(device, req, e.rec.Env) {
			continue
		}
		page = append(page, e.rec.Env)
	}
	remaining := 0
	for _, e := range list[pos:] {
		if !r.acknowledged(device, req, e.rec.Env) {
			remaining++
		}
	}
	return page, nextCursor, uint16(min(nextSkip, 0xFFFF)), remaining, nil
}

// syncWants reports whether a mailbox entry belongs in device's sync: MSGs
// to the callsign or this device, room messages, receipts for the
// callsign's own messages, and, with SENT, what the callsign sent. Delivery
// records are for nodes.
func syncWants(device envelope.Address, req envelope.Sync, e *envelope.Envelope) bool {
	base := device.Base()
	if isDeliveryRecord(e) {
		return false
	}
	dst := e.Destination()
	if e.Kind() == envelope.KindRCPT {
		return dst.Base() == base
	}
	switch {
	case dst == base || dst == device:
		return true
	case dst.IsStandard() && dst.Base() == base:
		return false // another device of this callsign
	case e.Source().Base() == base:
		return req.Sent()
	}
	return dst.IsRoom()
}

// acknowledged reports whether device has already had a MSG and the
// request does not want it again. Receipts are never acknowledged.
func (r *Station) acknowledged(device envelope.Address, req envelope.Sync, e *envelope.Envelope) bool {
	return !req.All() && e.Kind() == envelope.KindMSG && r.delivered.has(deliveryKey{id: e.StoreID(), device: device})
}
