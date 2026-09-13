# Pigeon: Rooms

*Part of Pigeon: An M17 Messaging System*

**Status:** Draft 0.1 — for discussion
**Scope:** Room addressing, subscription semantics, and the client ↔ node control packet type
**Depends on:** Pigeon: Architecture Overview; Pigeon: Message Envelope (MSG/RCPT)

## Terminology

This document is part of **Pigeon: An M17 Messaging System**. Throughout the Pigeon specifications:

- **Node** is the generic term for the store-and-forward daemon that sits between clients or radios and the rest of the network. The reference implementation is called **roost**, by analogy to where a bird spends the night: wherever it happens to be, not a fixed home. A callsign may have several nodes at once.
- **Home node** for a callsign means any node that has heard it and is holding state for it. It does not imply registration or permanence.
- **Public node** is a node with a reachable address that also relays for nodes behind NAT and serves internet-only clients.
- **Clock-in** is the informal name for a delivery receipt (RCPT), after the timing clock that records a racing pigeon's return.

## 1. Model

A room is a named topic. Messages sent to a room are delivered to every callsign subscribed to it.

Rooms are not objects. They have no owner, no creator, no home, and no explicit lifecycle. The only room state in the system is the set of **subscriptions**, each tying a callsign to a room name. A room "exists" while at least one subscription to it is live, and ceases to exist when the last one expires. This is the IRC channel model, chosen because it survives node failures and network partitions with no repair logic.

Consequences:

- Anyone can send to or subscribe to any room by name.
- There is no moderation at the room level in this version. Clients and nodes may maintain ignore lists by callsign, and node operators may decline to carry a room.
- Private or managed rooms are not supported in this version; §3.3 reserves address space for them.

## 2. Conventions

As in the Message Envelope specification, and see Terminology above. "Callsign" in this document means the *base callsign* (Architecture §3): the address with its device suffix removed. A subscription belongs to the base callsign and applies to all of its devices; a send from any device counts as that callsign.

## 3. Addressing

### 3.1 Room Names

A room name is 1 to 8 characters from the set `A`–`Z`, `0`–`9`, and `-`. Names are case-insensitive and canonicalized to upper case. `/` and `.` are valid base-40 characters but are excluded from room names to keep them typeable on constrained radio UIs and to leave room for future syntax.

Clients may display or accept a `#` prefix as a visual convention. The prefix is never part of the name or the encoding; a radio that presents rooms by menu selection never shows it.

### 3.2 Encoding

A room address is a 48-bit M17 address in the Extended range, which the M17 specification (`0xEE6B28000000`–`0xFFFFFFFFFFFE`) sets aside for application use:

```
RoomAddress = 0xEE6B28000000 + Base40(name)
```

where `Base40` is the M17 callsign encoding applied to the bare name: the first character is the least significant digit, exactly as for callsigns. Eight characters yield values below 40⁸ = `0x5F5E1000000`, so room addresses occupy `0xEE6B28000001` through `0xF46108FFFFFF`. The offset-zero address `0xEE6B28000000` (the empty name) is invalid.

Decoding is the inverse: subtract the base, then base-40 decode; a decoded string containing `/`, `.`, or a space, or longer than 8 characters, is not a valid room and the address must be treated as an unknown reserved address.

### 3.3 Reserved Address Space

Addresses from `0xF46109000000` through `0xFFFFFFFFFFFE` (the rest of the Extended range) are reserved by this specification for future use, in particular for managed or private rooms that carry membership and key material. Implementations must not deliver messages to these addresses as rooms.

### 3.4 Node-Callsign Rooms

A room whose name equals a node's callsign is that node's **local room**: "everyone currently using this node." It is an ordinary room in every respect except that the node auto-subscribes callsigns it hears (§4.4). It replaces the `@ALL` broadcast of legacy SMS clients, which is not supported for MSG.

## 4. Subscriptions

### 4.1 State

A subscription is a tuple `(callsign, room, last-refreshed)`. A node that processes a join or leave records it by storing a ROOM envelope in the callsign's inbox (Node Protocol §9), so any node that later homes the callsign learns its rooms from its inbox sweep. Membership of public rooms is therefore visible to anyone who can read the inbox, which in this version is everyone.

Opt-outs (§4.3) are likewise derived from the latest ROOM envelope per room, so they follow the callsign between nodes.

### 4.2 Joining

A callsign becomes subscribed to a room by any of:

- **Implicit join:** sending a MSG (or legacy SMS) whose Destination is the room. This is the primary path for radio users. It also clears any opt-out for that room.
- **Explicit join:** a ROOM control packet (§5) with op JOIN, or the legacy `/join` command (§6).
- **Auto-subscription** to a node-callsign room (§4.4).

A join to a room already subscribed refreshes `last-refreshed`.

### 4.3 Leaving

A callsign leaves a room by a ROOM control packet with op LEAVE or the legacy `/leave` command. The node removes the subscription and records an opt-out for `(callsign, room)`.

The opt-out prevents auto-subscription from re-adding the callsign. It is cleared only by the callsign sending to the room (implicit join) or explicitly joining it. Opt-outs expire with the callsign's other state (§4.5).

### 4.4 Auto-Subscription

When a node hears a callsign on its RF side or from a directly connected client, it subscribes that callsign to its own node-callsign room unless an opt-out exists. Nodes must not auto-subscribe callsigns to any other room.

### 4.5 Expiry

A subscription's `last-refreshed` is updated whenever the callsign is heard by the holding node or the callsign joins or sends to the room. Subscriptions expire 30 days after `last-refreshed`; nodes may configure a different value. When a callsign's last subscription expires its opt-outs may be discarded as well.

A room with no live subscriptions anywhere has no state anywhere and needs no cleanup.

### 4.6 Multiple Home Nodes

A callsign heard at several nodes has its subscriptions applied at each, all derived from the same inbox contents. The latest ROOM envelope per room wins; a LEAVE is a ROOM envelope like any other, so every node homing the callsign sees it after its next sweep and records the opt-out.

## 5. ROOM — Control Packet

Clients that speak the new protocol manage subscriptions with a dedicated packet type. Control packets are single-hop between a client and its node and are never forwarded, so source and destination come from the packet's LSF rather than the payload.

### 5.1 Layout

| Offset | Size | Field     | Description |
|-------:|-----:|-----------|-------------|
| 0      | 1    | Type      | Packet type = ROOM (`0x0A`, provisional; see Message Envelope §3) |
| 1      | 1    | Version   | `0x00` |
| 2      | 1    | Flags     | Bit 0 = SIGNED (reserved; no signature is defined for ROOM in this version); other bits reserved, must be 0 |
| 3      | 1    | Op        | See §5.2 |
| 4      | 4    | Timestamp | When the request was made; `0` = unknown (clock-less radio) |
| 8      | 1    | Count     | Number of 6-byte room addresses that follow |
| 9      | 6×n  | Rooms     | Room addresses |
| —      | var  | Note      | Optional UTF-8 text following the rooms list; replies only |

The layout is the same for requests and replies. Requests never carry a note.

The timestamp is what orders subscription state when a callsign's JOIN and LEAVE requests are stored in its inbox (Node Protocol §9): the latest per room wins. ROOM packets are not content-addressed, and no signature is defined for them in this version, so a node storing a request whose timestamp is `0` substitutes its own receipt time before storing it. The Flags byte exists so that a later version can sign stored JOIN/LEAVE records without changing the layout; once signatures exist, timestamp substitution will not be possible for signed requests and they will need a clock.

### 5.2 Ops

| Value  | Name   | Direction | Rooms | Meaning |
|-------:|--------|-----------|-------|---------|
| `0x00` | JOIN   | request   | 1+    | Subscribe the sending callsign to each listed room |
| `0x01` | LEAVE  | request   | 1+    | Unsubscribe and record opt-outs |
| `0x02` | LIST   | request   | 0     | Ask for the sending callsign's current subscriptions |
| `0x80` | OK     | reply     | 0+    | Request succeeded; for LIST, the rooms are the current subscriptions |
| `0x81` | REFUSED| reply     | 0+    | Request refused; the note says why. For a partially refused JOIN, the rooms are those that were refused. |

A node replies to every request. A reply to JOIN or LEAVE carries no rooms on success.

### 5.3 Rules

- A node stores each accepted JOIN or LEAVE request, as received (with timestamp filled if it was `0`), in the requesting callsign's inbox so that other nodes homing the callsign apply the same subscription state.
- A node must refuse JOIN for addresses outside the valid room range (§3.2) or that it has been configured not to carry.
- LEAVE for a room the callsign is not in succeeds and still records the opt-out.
- Control packets carry no message ID and generate no receipts.

## 6. Legacy Clients and Radios

Clients that speak only SMS interact with rooms as follows.

**Sending.** An SMS whose LSF Destination is a room address is treated as a MSG to that room (per the Message Envelope specification §6) and implicitly joins the sender.

**Receiving.** Room messages are delivered as SMS with the room address in the LSF Destination. Legacy radios will display these as messages addressed to an unfamiliar callsign-like string; that is acceptable.

**Control.** An SMS addressed to the node's own callsign whose text begins with `/` is a command:

| Command | Effect |
|---------|--------|
| `/join NAME [NAME…]` | As ROOM JOIN |
| `/leave NAME [NAME…]` | As ROOM LEAVE |
| `/rooms` | As ROOM LIST |

Names are matched case-insensitively and an optional leading `#` is ignored. The node replies with an SMS from its own callsign containing a short status line. Unrecognized commands get a one-line error. Commands are only honored from the RF side or from directly connected clients, never from forwarded traffic.

Since the node's callsign is also its local room name, an SMS to the node callsign that does *not* begin with `/` is a message to the local room, not a command.

## 7. Delivery

### 7.1 Routing

Each room is a gossipsub topic (Node Protocol §9). A node subscribes to the topic while it has any local subscriber; a room message is published to the topic and reaches only subscribed nodes. Gossipsub provides the fan-out, pruning, and dedup that keep room traffic from becoming a network-wide flood.

### 7.2 Storage

A home node queues a room message for each of its subscribed callsigns exactly as it queues unicast messages, subject to the message's TTL and a per-room cap (default 200 messages, configurable). The room's history is therefore held wherever it has subscribers and nowhere else.

### 7.3 Backlog

When a callsign joins a room at a node that holds no history for it, the node may query a room archive (a public node subscribed to the room that stores its messages; Node Protocol §9) for messages still within TTL. Duplicates are discarded by message ID.

### 7.4 RF

A node transmits each room message on RF at most once, regardless of how many local callsigns are subscribed. Radios filter by their own subscription list, which a new-protocol radio can obtain with ROOM LIST.

### 7.5 Receipts

No receipts are generated for room messages except REJECTED, per the Message Envelope specification.

## 8. Open Questions

1. **Room list on the radio.** A ROOM LIST reply of a few dozen rooms is fine; a radio that wants to *discover* rooms has no mechanism here. Is a "rooms this node carries" query wanted, or is discovery a social problem?
2. **Backlog on RF.** Delivering backlog to a radio user means transmitting old messages on the repeater. Is the right default "backlog to IP clients only," with RF getting only new traffic?
3. **Extended-range use.** The M17 specification opens the Extended range to applications without a registry, so nothing prevents another application from using the same values for something else. Worth raising with the M17 working group once the design settles.

## 9. Resolved

- **Subscription state** is stored as ROOM envelopes in the callsign's inbox and applied by every node that homes it; room fan-out is a gossipsub topic per room.
- **Rooms are subscription-only,** with no ownership or explicit lifecycle.
- **Implicit join on send.** Sending to a room subscribes the sender.
- **Explicit leave is sticky** via a recorded opt-out, cleared only by rejoining or sending.
- **Node-callsign rooms** with auto-subscription replace `@ALL`.
- **Room names** are 1–8 characters from `A–Z`, `0–9`, `-`, encoded as a base-40 offset into the Extended range; `#` is a display convention only.
