package qtcd

import (
	"net"
	"time"

	"github.com/jancona/m17"
)

// Current M17 reflectors (mrefd 1.0.0 and later) forward traffic to its
// real destination; legacy ones (all urfd, and mrefd before 1.0.0) forward
// only streams addressed to their reflector and module, and support
// neither PARROT nor packet mode. A gateway tells them apart by sending
// PARROT a probe when it links (m17 m17.Probe), and stops sending packets
// to a reflector that doesn't answer.
//
// To its clients the node is a current reflector: it answers PARROT
// packets itself, so a gateway keeps sending it packets whatever the
// upstream, and on a messaging-only module that has no upstream at all.
// Toward its upstream it does what a gateway does: it probes on each link,
// and if the upstream is legacy, readdresses broadcast streams to the
// upstream reflector and module and stops forwarding packets.

// The upstream is legacy if probeTries probes, probeInterval apart, go
// unanswered (as m17 inet.Client decides for a gateway).
const probeTries = 3

var probeInterval = 1500 * time.Millisecond

// answerParrot answers a packet to PARROT, b, as a current reflector does:
// sent back to the client readdressed to broadcast. b's CRCs have been
// checked.
func (s *inetSession) answerParrot(b []byte) {
	p, err := m17.NewPacketFromBytes(b[4:])
	if err != nil {
		return
	}
	reply := m17.ParrotReply(p)
	s.face.send(s.client, append([]byte(magicM17P), reply.ToBytes()...))
}

// upstreamName returns the upstream reflector and module as a callsign,
// for readdressing broadcasts to a legacy upstream, or nil if the
// reflector is given as host:port and so has no name.
func upstreamName(mod ModuleConfig) *m17.EncodedCallsign {
	if _, _, err := net.SplitHostPort(mod.Reflector); err == nil {
		return nil
	}
	e, err := m17.EncodeCallsign(m17.NormalizeCallsignModule(mod.Reflector + " " + string(mod.Module)))
	if err != nil {
		return nil
	}
	return e
}

// startProbe sends the upstream a new probe, from the client's callsign as
// the client would send it.
func (s *inetSession) startProbe() {
	probe := m17.NewProbe()
	s.mu.Lock()
	if s.probeTimer != nil {
		s.probeTimer.Stop()
	}
	s.upLegacy = false
	s.probe = &probe
	s.probeSent = 0
	s.mu.Unlock()
	s.sendProbe(&probe)
}

// sendProbe sends probe, unless it has been answered or superseded or the
// session has closed, and resends it after probeInterval. Once
// probeTries have gone unanswered, the upstream is legacy.
func (s *inetSession) sendProbe(probe *m17.Probe) {
	s.mu.Lock()
	if s.closed || probe != s.probe || s.upLegacy {
		s.mu.Unlock()
		return
	}
	if s.probeSent == probeTries {
		s.upLegacy = true
		s.mu.Unlock()
		s.face.log.Info("upstream did not answer PARROT; treating it as a legacy reflector",
			"client", s.client, "upstream", s.mod.Reflector, "upstream_module", string(s.mod.Module))
		return
	}
	s.probeSent++
	s.probeTimer = time.AfterFunc(probeInterval, func() { s.sendProbe(probe) })
	s.mu.Unlock()
	p := probe.Packet(encodedCallsign(s.callsign))
	s.forwardUp(append([]byte(magicM17P), p.ToBytes()...))
}

// probeReply reports whether b, an M17P datagram from the upstream, is the
// reply to the probe, and if so, notes the upstream is current.
func (s *inetSession) probeReply(b []byte) bool {
	p, err := m17.NewPacketFromBytes(b[4:])
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probe == nil || !s.probe.IsReply(p) {
		return false
	}
	s.probeTimer.Stop()
	if s.upLegacy {
		s.face.log.Info("late reply from upstream to PARROT; treating it as a current reflector", "client", s.client, "upstream", s.mod.Reflector)
	} else {
		s.face.log.Debug("upstream answered PARROT: current reflector", "client", s.client, "upstream", s.mod.Reflector)
	}
	s.upLegacy = false
	return true
}

// forwardStream forwards a client stream frame upstream, readdressing a
// broadcast for a legacy upstream. Whether the upstream is legacy is
// decided once per stream, so a probe finishing mid-stream cannot
// readdress it partway through. Called only from the face's read loop.
func (s *inetSession) forwardStream(b []byte) {
	if s.up == nil {
		return
	}
	sd, err := m17.NewStreamDatagramFromBytes(b)
	if err != nil {
		s.forwardUp(b)
		return
	}
	if !s.streamSeen || sd.StreamID != s.streamID {
		s.streamSeen, s.streamID, s.streamLogged = true, sd.StreamID, false
		s.mu.Lock()
		s.streamLegacy = s.upLegacy
		s.mu.Unlock()
	}
	if !s.streamLegacy {
		s.forwardUp(b)
		return
	}
	broadcast := m17.IsBroadcast(sd.LSF.Dst)
	if broadcast && s.upName != nil {
		sd.LSF.Dst = *s.upName
		s.forwardUp(sd.ToBytes())
		return
	}
	if !s.streamLogged {
		s.streamLogged = true
		if broadcast {
			s.face.log.Info("legacy upstream given as host:port; broadcast stream not readdressed and will probably be dropped",
				"client", s.client, "upstream", s.mod.Reflector, "stream", sd.StreamID)
		} else {
			s.face.log.Info("legacy upstream will probably drop a stream not addressed to broadcast",
				"client", s.client, "upstream", s.mod.Reflector, "stream", sd.StreamID, "dst", sd.LSF.Dst.Callsign())
		}
	}
	s.forwardUp(b)
}

// forwardPacket forwards a client packet upstream, unless the upstream is
// legacy: legacy reflectors don't support packet mode.
func (s *inetSession) forwardPacket(b []byte) {
	s.mu.Lock()
	legacy := s.upLegacy
	s.mu.Unlock()
	if legacy {
		s.face.log.Info("packet not forwarded: legacy upstream doesn't support packet mode", "client", s.client, "upstream", s.mod.Reflector)
		return
	}
	s.forwardUp(b)
}
