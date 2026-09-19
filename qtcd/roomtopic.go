package qtcd

import (
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
)

// roomTopics keeps a gossipsub topic joined for every room with a local
// subscriber (rooms spec §7.1, node protocol §9).
type roomTopics struct {
	r  *Station
	mu sync.Mutex
	// joined maps room address to its topic and subscription.
	joined map[envelope.Address]*roomTopic
}

type roomTopic struct {
	topic *pubsub.Topic
	sub   *pubsub.Subscription
}

func newRoomTopics(r *Station) *roomTopics {
	return &roomTopics{r: r, joined: map[envelope.Address]*roomTopic{}}
}

// TopicName is the gossipsub topic for a room.
func TopicName(room envelope.Address) string {
	name, _ := room.RoomName()
	return RoomTopicPrefix + name
}

// run reconciles joined topics with ActiveRooms every few seconds.
func (rt *roomTopics) run() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		rt.reconcile()
		select {
		case <-rt.r.ctx.Done():
			rt.mu.Lock()
			for room, jt := range rt.joined {
				jt.sub.Cancel()
				jt.topic.Close()
				delete(rt.joined, room)
			}
			rt.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

func (rt *roomTopics) reconcile() {
	want := map[envelope.Address]bool{}
	for _, room := range rt.r.subs.ActiveRooms() {
		want[room] = true
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for room, jt := range rt.joined {
		if !want[room] {
			jt.sub.Cancel()
			if err := jt.topic.Close(); err != nil {
				rt.r.log.Debug("close room topic", "room", room, "err", err)
			}
			delete(rt.joined, room)
			rt.r.log.Info("left room topic", "room", room)
		}
	}
	for room := range want {
		if rt.joined[room] != nil {
			continue
		}
		topic, err := rt.r.ps.Join(TopicName(room))
		if err != nil {
			rt.r.log.Warn("join room topic", "room", room, "err", err)
			continue
		}
		sub, err := topic.Subscribe()
		if err != nil {
			topic.Close()
			rt.r.log.Warn("subscribe room topic", "room", room, "err", err)
			continue
		}
		rt.joined[room] = &roomTopic{topic: topic, sub: sub}
		rt.r.log.Info("joined room topic", "room", room)
		rt.r.go_(func() { rt.receive(room, sub) })
	}
}

// receive handles room messages from the network: parse, check the
// destination is this room, store for local subscribers, deliver.
func (rt *roomTopics) receive(room envelope.Address, sub *pubsub.Subscription) {
	for {
		m, err := sub.Next(rt.r.ctx)
		if err != nil {
			return
		}
		if m.GetFrom() == rt.r.host.ID() {
			continue
		}
		e, err := envelope.Parse(m.Data)
		if err != nil {
			rt.r.log.Debug("bad room message", "room", room, "from", m.GetFrom(), "err", err)
			continue
		}
		if e.Type() != envelope.TypeMSG || e.Destination() != room {
			rt.r.log.Debug("room message for wrong room", "room", room, "envelope", e)
			continue
		}
		if !rt.r.markSeenRoomMsg(e) {
			continue
		}
		rt.r.storeRoomMessage(e)
		rt.r.deliverLocal(e)
	}
}

// markSeenRoomMsg dedups room messages by ID using the delivery table with
// the room address standing in for a device.
func (r *Station) markSeenRoomMsg(e *envelope.Envelope) bool {
	return r.markDelivered(e, e.Destination())
}

// publish sends a room MSG to the room's topic, joining it first if needed
// (the sender is a subscriber by implicit join, so reconcile would do it
// shortly anyway).
func (rt *roomTopics) publish(room envelope.Address, e *envelope.Envelope) {
	rt.reconcile()
	rt.mu.Lock()
	jt := rt.joined[room]
	rt.mu.Unlock()
	if jt == nil {
		rt.r.log.Warn("no topic for room", "room", room)
		return
	}
	rt.r.markSeenRoomMsg(e)
	if err := jt.topic.Publish(rt.r.ctx, e.Bytes()); err != nil {
		rt.r.log.Warn("publish room message", "room", room, "err", err)
	}
}
