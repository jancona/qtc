# Pigeon: An M17 Messaging System

## Architecture Overview

**Status:** Draft 0.1 — for discussion

## 1. Goals

- Store-and-forward text messaging between M17 users, by callsign and by room.
- Works radio-to-radio through repeaters and hotspots, and from internet clients, with the same message reaching all of a user's devices.
- Legacy radios and gateways participate unmodified, using the existing M17 SMS packet type and the standard [M17_inet](https://github.com/M17-Project/M17_inet) protocol.
- No single operator can take the network down, and anyone can run any piece of it.
- Scale limits and spoofing resistance may be deferred, but nothing in the design should preclude fixing them later.

Non-goals for this version: voice routing, message confidentiality, private rooms, moderation beyond ignore lists.

## 2. Components

**Radios and clients.** Anything that speaks M17. Legacy devices send SMS (type `0x05`) and are treated as terminals: they see what is transmitted to them and nothing else. Native devices speak the Pigeon envelope (MSG/RCPT/ROOM) and can dedup, sync, and acknowledge.

**roost** is the node daemon. It runs on a hotspot, a repeater controller, a VPS, or embedded in a client. It presents a standard [M17_inet](https://github.com/M17-Project/M17_inet) reflector interface on the local side, so any gateway links to it exactly as it would to a reflector, and it proxies voice and unrelated packet traffic to the user's chosen upstream reflector transparently. It intercepts messaging traffic and speaks the Pigeon node protocol to the rest of the network.

Every roost is a **node**. A node may have capabilities:

| Capability | Meaning |
|---|---|
| `public` | Has a reachable address; accepts inbound peer connections |
| `relay` | Relays connections for nodes behind NAT |
| `inbox` | Hosts mailboxes for callsigns |
| `clients` | Accepts remote gateways and clients over the internet using the M17_inet protocol |

A **public node** is a roost with `public` and usually the rest. A hotspot behind NAT has none of them and is simply a roost.

**Reflectors** are unchanged and outside Pigeon. They remain the voice fabric and the fallback for messaging a callsign that has no Pigeon presence at all.

## 3. Identity

**Callsigns.** The *base callsign* is the M17 address with its device suffix removed: everything before the first space or `-`. (Device suffixes are a community convention, not part of the M17 specification.) (`/` is part of some callsigns and is not a separator.) All messaging state is keyed by base callsign; the full address is retained as the *device*. A destination with a suffix targets that device only; without one, all devices. Node callsigns (e.g. `K1XYZ  R`) are used whole and never treated as users.

**Nodes** have an ECDSA secp256r1 key pair. The node ID is derived from the public key, and it is what every claim in the network is attributed to. The node callsign is a claim; the key is the identity.

**Users** have no keys in this version. A future user certificate binds a key to a base callsign, and the envelope already reserves a signature slot for it. Until then, a user's authority is exercised by their loft (§5).

## 4. Trust

- **Message integrity** comes from envelope signatures, when present. No node can forge or alter a signed message.
- **Routing trust** comes from node identity, qualified by how the node observed a callsign (`heard via` RF, local client, or internet client).
- **Confidentiality** is not provided by anything in the system. Content is plaintext at every node and on RF. TLS between nodes authenticates nodes and nothing more.

## 5. Where Messages Live

Each base callsign has an **inbox**: a small fixed set of public nodes (target size 2–3) that store its messages, both received and sent, for the message's TTL. Senders write to every member of the set and retry until all accept. Any roost that hears the callsign reads from the set, delivers locally, and keeps the members reconciled with each other. Inbox nodes are pure storage with two operations, put and query; they never talk to each other.

Each base callsign has a **loft**: the one roost that speaks for it, authoritative for its inbox record and policy. In practice it is the user's own hotspot. Any roost may read and deliver; only the loft repairs and migrates the inbox record, unless the loft has been silent long enough that another roost may claim it. When user keys arrive the user replaces the loft as the authority.

A **roost** in the general sense is any node currently holding state for a callsign because it heard it. A callsign may have many roosts and one loft.

## 6. Message Flow

1. A user sends a message. Their roost wraps a legacy SMS in an envelope, or accepts a native envelope; the message ID is derived from the envelope contents.
2. The roost looks up the recipient's inbox record. If none exists (first-time recipient), it creates one on the sender's behalf, to be handed off when a roost that actually hears the recipient appears.
3. The roost puts the envelope on every inbox member and on the sender's own inbox, retrying until all accept. If the sender requested it, the roost issues a QUEUED receipt.
4. Every roost that homes the recipient has the recipient's inbox nodes on watch and is notified immediately. Each transmits once on RF or delivers to its clients, and issues TRANSMITTED with the recipient's last-heard time at that roost.
5. A native client that receives the message issues DELIVERED, which is put on the sender's inbox like any other envelope.
6. A roost newly homing the recipient sweeps the inbox: queries all members since its last sync, unions, fills gaps, and replays recent messages that carry no DELIVERED.

Rooms use gossipsub topics instead of inboxes; see the Rooms specification.

## 7. Substrate

The node protocol runs on go-libp2p: TLS transport, peer identity from the node key, DHT for peer discovery and inbox-record lookup, circuit relay for nodes behind NAT, and gossipsub for presence and room traffic. Pigeon defines only what goes over those channels and one request/response protocol for inbox storage.

## 8. Specifications

| Document | Covers |
|---|---|
| Pigeon: Message Envelope | MSG and RCPT packet types; message IDs; TTL; signatures |
| Pigeon: Rooms | Room addressing, subscriptions, ROOM control type |
| Pigeon: Node Protocol | Identity, peering, presence, inbox records, storage protocol, delivery |

## 9. Milestones

1. **Spike.** Two roosts on separate home networks behind NAT, one public node on a VPS. Presence over gossipsub, one room topic, one inbox with put/query. Measure binary size and memory on a Pi Zero 2. Decide whether libp2p earns its weight.
2. **Proxy reflector.** A roost in front of an unmodified WPSD hotspot, generated hosts file, voice unaffected.
3. **Legacy messaging end to end.** SMS from an OpenRTX radio through a hotspot roost to a GUI client and back.
4. **Native envelope.** OpenRTX speaks MSG/RCPT; receipts and dedup work.
5. **Loft and repair.** Inbox record lifecycle, first-time recipient, node failure and recovery.

## 10. Open Questions

1. **Presence establishment.** A roost homes a callsign when it hears it. How a radio makes itself heard on arrival (keying up, a beacon, something in OpenRTX) shapes the user experience more than anything in the routing. Deliberately unresolved.
2. **Thin clients.** A phone or browser client can be a libp2p peer over WebSocket, or public nodes can expose the storage protocol directly. To be decided after the spike.
3. **Quotas and abuse.** Anyone can create an inbox for any callsign and put to it. Per-node quotas are assumed; nothing is specified.
