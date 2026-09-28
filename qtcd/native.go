package qtcd

import (
	"math/rand/v2"
	"slices"
	"time"

	"github.com/jancona/qtc/envelope"
)

// Native devices on the client face (docs/qtc-client.md). A device that
// sends a QTC payload is native; one that sends SMS is legacy (§2). A node
// sends a native device MSGs and receipts as QTC payloads and keeps
// resending each MSG until the device acknowledges it with DELIVERED or ACK
// (§4). A device fetches its backlog with SYNC (§5). On RF, room messages go
// out once per gateway, unacknowledged, and summaries let radios FETCH what
// they missed (§6.1).

// Native client timing and sizes (client spec §4.3, §6.2), variables so
// tests can shorten them.
var (
	ackTimeoutRF   = 5 * time.Second
	ackJitterRF    = 2 * time.Second
	ackTimeoutInet = 2 * time.Second
	ackRetries     = 3
	// ackSpacingRF is added per packet queued ahead on a gateway, which
	// transmits them one at a time.
	ackSpacingRF = time.Second

	summaryEvery   = 8
	summaryIdle    = 5 * time.Second
	summaryRepeat  = 60 * time.Second
	summarySize    = 16
	summaryMaxAge  = 30 * time.Minute
	fetchDedup     = 3 * time.Second
	syncPageRF     = 5
	syncPageInet   = 50
	notifyQuiet    = 30 * time.Second
	acceptedMemory = 10 * time.Minute
	nativeTick     = 250 * time.Millisecond
)

// deliverResult is what the face did with an envelope for a device.
type deliverResult int

const (
	deliverNone    deliverResult = iota // not sent: no link
	deliverSent                         // sent; counts as delivered now
	deliverPending                      // sent to a native device; the face reports the acknowledgement or its loss
)

// pendingKey names one MSG awaiting one device's acknowledgement.
type pendingKey struct {
	id     envelope.ID
	device envelope.Address
}

type pendingMsg struct {
	e        *envelope.Envelope
	attempts int
	next     time.Time
	fresh    bool // live delivery (deliverOne), not a sync page
}

// roomTx is a room message a gateway session transmitted on RF.
type roomTx struct {
	e    *envelope.Envelope
	at   time.Time
	sent time.Time // last transmission, including FETCH retransmits
}

// nativeState is a session's native-client state, guarded by the session's
// mu.
type nativeState struct {
	pending  map[pendingKey]*pendingMsg
	accepted map[envelope.ID]time.Time // MSGs taken from the client, so a retry is re-ACKed, not resent
	lastSync map[envelope.Address]time.Time

	// RF gateways only: room summaries (client spec §6.1).
	roomLog      []roomTx
	sinceSummary int
	lastRoomTx   time.Time
	lastSummary  *envelope.Envelope
	summaryAt    time.Time
	repeatAt     time.Time // zero: no repeat due
}

func newNativeState() nativeState {
	return nativeState{
		pending:  map[pendingKey]*pendingMsg{},
		accepted: map[envelope.ID]time.Time{},
		lastSync: map[envelope.Address]time.Time{},
	}
}

// setNative records whether a device's latest packet was QTC or SMS.
func (f *inetFace) setNative(device envelope.Address, native bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if native {
		f.native[device] = true
	} else {
		delete(f.native, device)
	}
}

// isNative reports whether a device is native (client spec §2).
func (f *inetFace) isNative(device envelope.Address) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.native[device]
}

// deliverNative sends a MSG or RCPT to a native device.
func (s *inetSession) deliverNative(device envelope.Address, e *envelope.Envelope) deliverResult {
	if e.Kind() != envelope.KindMSG {
		s.face.log.Debug("receipt to native client", "client", s.client, "device", device, "envelope", e)
		s.sendEnvelope(e.Destination(), e.Source(), e)
		return deliverSent
	}
	if e.Destination().IsRoom() && s.via == ViaRF {
		s.transmitRoom(e)
		return deliverSent
	}
	s.face.log.Info("delivering MSG to native client", "client", s.client, "device", device, "envelope", e)
	s.track(device, e, true, 0)
	return deliverPending
}

// sendEnvelope frames a QTC payload for the client with the given LSF
// addresses.
func (s *inetSession) sendEnvelope(dst, src envelope.Address, e *envelope.Envelope) {
	s.face.send(s.client, buildPacketDatagram(dst, src, e.Bytes()))
}

// sendControl sends a control packet (SYNC, ACK, ROOM) from the node to a
// device (client spec §2).
func (s *inetSession) sendControl(device envelope.Address, e *envelope.Envelope) {
	s.sendEnvelope(device, s.face.core.inetCallsign(), e)
}

// ackTimeout is how long to wait for an acknowledgement on this link, for a
// packet with queued packets ahead of it.
func (s *inetSession) ackTimeout(queued int) time.Duration {
	if s.via != ViaRF {
		return ackTimeoutInet
	}
	return ackTimeoutRF + time.Duration(queued)*ackSpacingRF + time.Duration(rand.Int64N(int64(ackJitterRF)+1))
}

// track sends a MSG to a device and keeps resending it until acknowledged.
func (s *inetSession) track(device envelope.Address, e *envelope.Envelope, fresh bool, queued int) {
	k := pendingKey{id: e.ID(), device: device}
	s.mu.Lock()
	if p, ok := s.nat.pending[k]; ok {
		p.fresh = p.fresh || fresh
		s.mu.Unlock()
		return // already on its way
	}
	s.nat.pending[k] = &pendingMsg{e: e, attempts: 1, next: time.Now().Add(s.ackTimeout(queued)), fresh: fresh}
	s.mu.Unlock()
	s.sendEnvelope(e.Destination(), e.Source(), e)
}

// onAck handles a device's DELIVERED or ACK for a message (client spec §4).
// rcpt is the DELIVERED, or nil for an ACK.
func (s *inetSession) onAck(device envelope.Address, id envelope.ID, rcpt *envelope.Envelope) {
	k := pendingKey{id: id, device: device}
	s.mu.Lock()
	p := s.nat.pending[k]
	delete(s.nat.pending, k)
	s.mu.Unlock()
	if p == nil {
		s.face.log.Debug("acknowledgement for nothing pending", "client", s.client, "device", device, "id", id)
		return
	}
	s.face.log.Debug("acknowledged", "client", s.client, "device", device, "id", id)
	s.face.core.inetAcked(device, p.e, rcpt, p.fresh)
}

// retry resends overdue MSGs, and gives up on those out of retries.
func (s *inetSession) retry(now time.Time) {
	type lostMsg struct {
		device envelope.Address
		p      *pendingMsg
	}
	var resend []*envelope.Envelope
	var lost []lostMsg
	s.mu.Lock()
	for k, p := range s.nat.pending {
		if now.Before(p.next) {
			continue
		}
		if p.attempts > ackRetries {
			delete(s.nat.pending, k)
			lost = append(lost, lostMsg{k.device, p})
			continue
		}
		p.attempts++
		p.next = now.Add(s.ackTimeout(0))
		resend = append(resend, p.e)
	}
	s.mu.Unlock()
	for _, e := range resend {
		s.face.log.Debug("resending unacknowledged MSG", "client", s.client, "envelope", e)
		s.sendEnvelope(e.Destination(), e.Source(), e)
	}
	for _, l := range lost {
		s.face.log.Info("native device did not acknowledge; holding", "client", s.client, "device", l.device, "id", l.p.e.ID())
		s.face.core.inetLost(l.device, l.p.e, l.p.fresh)
	}
}

// ingestNative takes a QTC payload of any kind from a native device.
func (s *inetSession) ingestNative(pf packetFrame, e *envelope.Envelope) {
	device := pf.src
	switch e.Kind() {
	case envelope.KindMSG:
		s.ingestMsg(device, e)
	case envelope.KindRCPT:
		rc, _ := e.Rcpt()
		if rc.Status() == envelope.StatusDelivered && e.Source().Base() == device.Base() {
			s.onAck(device, rc.MessageID(), e)
			return
		}
		if err := s.face.core.inetSend(e); err != nil {
			s.face.log.Info("receipt from client not sent", "client", s.client, "envelope", e, "err", err)
		}
	case envelope.KindACK:
		a, _ := e.Ack()
		for _, id := range a.IDs() {
			s.onAck(device, id, nil)
		}
	case envelope.KindSYNC:
		y, _ := e.Sync()
		switch y.Op() {
		case envelope.SyncRequest:
			s.syncRequest(device, y)
		case envelope.SyncFetch:
			s.fetch(y.ShortIDs())
		default:
			s.face.log.Debug("ignoring SYNC from client", "client", s.client, "op", y.Op())
		}
	}
}

// ingestMsg sends a device's MSG and acknowledges it, or refuses it with
// REJECTED (client spec §4). A retry of one already taken is acknowledged
// again without being sent twice.
func (s *inetSession) ingestMsg(device envelope.Address, e *envelope.Envelope) {
	node := s.face.core.inetCallsign()
	if e.Source().Base() != device.Base() {
		s.reject(device, e, "source does not match sender")
		return
	}
	now := time.Now()
	s.mu.Lock()
	_, dup := s.nat.accepted[e.ID()]
	s.mu.Unlock()
	if !dup {
		if err := s.face.core.inetSend(e); err != nil {
			s.face.log.Info("MSG from native client refused", "client", s.client, "envelope", e, "err", err)
			s.reject(device, e, err.Error())
			return
		}
		s.mu.Lock()
		s.nat.accepted[e.ID()] = now
		s.mu.Unlock()
	}
	ack, err := envelope.BuildAck([]envelope.ID{e.ID()})
	if err != nil {
		s.face.log.Error("build ACK", "err", err)
		return
	}
	s.sendEnvelope(device, node, ack)
}

func (s *inetSession) reject(device envelope.Address, e *envelope.Envelope, why string) {
	node := s.face.core.inetCallsign()
	rc, err := envelope.BuildRcpt(node, e.Source(), e.ID(), envelope.StatusRejected, uint32(time.Now().Unix()), 0, why)
	if err != nil {
		s.face.log.Error("build REJECTED", "err", err)
		return
	}
	s.sendEnvelope(device, node, rc)
}

// syncRequest answers a SYNC REQUEST with a PAGE and the page's packets
// (client spec §5.1).
func (s *inetSession) syncRequest(device envelope.Address, y envelope.Sync) {
	limit := syncPageInet
	if s.via == ViaRF {
		limit = syncPageRF
	}
	if n := y.Count(); n > 0 && n < limit {
		limit = n
	}
	s.mu.Lock()
	s.nat.lastSync[device] = time.Now()
	s.mu.Unlock()
	page, cursor, skip, remaining, err := s.face.core.inetSync(device, y, limit)
	if err != nil {
		s.face.log.Info("SYNC refused", "client", s.client, "device", device, "err", err)
		if r, err := envelope.BuildSyncRefused(err.Error()); err == nil {
			s.sendControl(device, r)
		}
		return
	}
	reply, err := envelope.BuildSync(envelope.SyncPage, 0, cursor, skip, len(page), uint16(min(remaining, 0xFFFF)))
	if err != nil {
		s.face.log.Error("build PAGE", "err", err)
		return
	}
	s.face.log.Info("sync page", "client", s.client, "device", device, "count", len(page), "remaining", remaining)
	s.sendControl(device, reply)
	for i, e := range page {
		if e.Kind() == envelope.KindMSG {
			s.track(device, e, false, i+1)
		} else {
			s.sendEnvelope(e.Destination(), e.Source(), e)
		}
	}
}

// notify tells a native device how many messages are waiting, instead of
// replaying them (client spec §5.3). A device that has just synced is left
// alone.
func (s *inetSession) notify(device envelope.Address, waiting int) {
	s.mu.Lock()
	recent := time.Since(s.nat.lastSync[device]) < notifyQuiet
	s.mu.Unlock()
	if recent {
		return
	}
	n, err := envelope.BuildSync(envelope.SyncNotify, 0, 0, 0, 0, uint16(min(waiting, 0xFFFF)))
	if err != nil {
		s.face.log.Error("build NOTIFY", "err", err)
		return
	}
	s.face.log.Info("notifying native device of waiting messages", "client", s.client, "device", device, "waiting", waiting)
	s.sendControl(device, n)
}

// notify finds the session that heard a native device and notifies it.
func (f *inetFace) notify(device envelope.Address, waiting int) bool {
	f.mu.Lock()
	s := f.devices[device]
	f.mu.Unlock()
	if s == nil {
		return false
	}
	s.notify(device, waiting)
	return true
}

// transmitRoom sends a room MSG once on this gateway, to the room address,
// and logs it for summaries (client spec §6.1).
func (s *inetSession) transmitRoom(e *envelope.Envelope) {
	now := time.Now()
	s.mu.Lock()
	for _, tx := range s.nat.roomLog {
		if tx.e.ID() == e.ID() {
			s.mu.Unlock()
			return // already transmitted on this gateway
		}
	}
	s.nat.roomLog = append(s.nat.roomLog, roomTx{e: e, at: now, sent: now})
	if n := len(s.nat.roomLog) - summarySize; n > 0 {
		s.nat.roomLog = slices.Delete(s.nat.roomLog, 0, n)
	}
	s.nat.sinceSummary++
	s.nat.lastRoomTx = now
	due := s.nat.sinceSummary >= summaryEvery
	s.mu.Unlock()
	s.face.log.Info("transmitting room MSG to native radios", "client", s.client, "envelope", e)
	s.sendEnvelope(e.Destination(), e.Source(), e)
	if due {
		s.summarize(false)
	}
}

// summarize sends a SUMMARY of the room messages this gateway transmitted
// recently, if any native radio in reach wants one of those rooms.
// burstEnd schedules the one repeat.
func (s *inetSession) summarize(burstEnd bool) {
	now := time.Now()
	s.mu.Lock()
	s.nat.sinceSummary = 0
	s.nat.repeatAt = time.Time{}
	var groups []envelope.SummaryGroup
	rooms := map[envelope.Address]int{}
	for _, tx := range s.nat.roomLog {
		if now.Sub(tx.at) > summaryMaxAge {
			continue
		}
		room := tx.e.Destination()
		i, ok := rooms[room]
		if !ok {
			i = len(groups)
			rooms[room] = i
			groups = append(groups, envelope.SummaryGroup{Room: room})
		}
		groups[i].IDs = append(groups[i].IDs, tx.e.ID().Short())
	}
	s.mu.Unlock()
	if len(groups) == 0 || !s.audience(rooms) {
		return
	}
	sum, err := envelope.BuildSyncSummary(groups)
	if err != nil {
		s.face.log.Error("build SUMMARY", "err", err)
		return
	}
	s.mu.Lock()
	s.nat.lastSummary = sum
	s.nat.summaryAt = now
	if burstEnd {
		s.nat.repeatAt = now.Add(summaryRepeat)
	}
	s.mu.Unlock()
	s.face.log.Debug("room summary", "client", s.client, "summary", sum)
	s.sendEnvelope(envelope.Broadcast, s.face.core.inetCallsign(), sum)
}

// audience reports whether a native device in reach on this gateway is
// subscribed to any of the rooms.
func (s *inetSession) audience(rooms map[envelope.Address]int) bool {
	var natives []envelope.Address
	s.face.mu.Lock()
	for d, owner := range s.face.devices {
		if owner == s && s.face.native[d] {
			natives = append(natives, d)
		}
	}
	s.face.mu.Unlock()
	for _, d := range natives {
		if !s.face.core.inetReachable(d) {
			continue
		}
		for room := range rooms {
			if s.face.core.inetSubscribed(d, room) {
				return true
			}
		}
	}
	return false
}

// summaryTick sends the burst-end summary and its repeat when due.
func (s *inetSession) summaryTick(now time.Time) {
	if s.via != ViaRF {
		return
	}
	s.mu.Lock()
	burstEnd := s.nat.sinceSummary > 0 && now.Sub(s.nat.lastRoomTx) >= summaryIdle
	var repeat *envelope.Envelope
	if !s.nat.repeatAt.IsZero() && !now.Before(s.nat.repeatAt) {
		if s.nat.lastRoomTx.Before(s.nat.summaryAt) {
			repeat = s.nat.lastSummary
		}
		s.nat.repeatAt = time.Time{}
	}
	s.mu.Unlock()
	if burstEnd {
		s.summarize(true)
	} else if repeat != nil {
		s.face.log.Debug("repeating room summary", "client", s.client)
		s.sendEnvelope(envelope.Broadcast, s.face.core.inetCallsign(), repeat)
	}
}

// fetch retransmits the room messages a radio asked for (client spec §6.1),
// once each however many radios ask within fetchDedup.
func (s *inetSession) fetch(ids []envelope.ShortID) {
	now := time.Now()
	want := map[envelope.ShortID]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var resend []*envelope.Envelope
	s.mu.Lock()
	for i := range s.nat.roomLog {
		tx := &s.nat.roomLog[i]
		if want[tx.e.ID().Short()] && now.Sub(tx.sent) >= fetchDedup {
			tx.sent = now
			resend = append(resend, tx.e)
		}
	}
	s.mu.Unlock()
	for _, e := range resend {
		s.face.log.Info("retransmitting fetched room MSG", "client", s.client, "envelope", e)
		s.sendEnvelope(e.Destination(), e.Source(), e)
	}
}

// expireAccepted forgets MSGs taken long enough ago that no retry of them
// can still arrive.
func (s *inetSession) expireAccepted(now time.Time) {
	s.mu.Lock()
	for id, at := range s.nat.accepted {
		if now.Sub(at) > acceptedMemory {
			delete(s.nat.accepted, id)
		}
	}
	s.mu.Unlock()
}

// nativeLoop drives retries and summaries for every session.
func (f *inetFace) nativeLoop() {
	defer f.wg.Done()
	t := time.NewTicker(nativeTick)
	defer t.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return
		case now := <-t.C:
			f.mu.Lock()
			sessions := make([]*inetSession, 0, len(f.sessions))
			for _, s := range f.sessions {
				sessions = append(sessions, s)
			}
			f.mu.Unlock()
			for _, s := range sessions {
				s.retry(now)
				s.summaryTick(now)
				s.expireAccepted(now)
			}
		}
	}
}
