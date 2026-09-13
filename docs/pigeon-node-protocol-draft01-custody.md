# Pigeon: Node Protocol

*Part of Pigeon: An M17 Messaging System*

**Status:** Draft 0.1 — for discussion
**Scope:** Node ↔ node transport, identity, peering, gossip, delivery, and backlog
**Depends on:** Pigeon: Message Envelope (MSG/RCPT); Pigeon: Rooms

## Terminology

This document is part of **Pigeon: An M17 Messaging System**. Throughout the Pigeon specifications:

- **Node** is the generic term for the store-and-forward daemon that sits between clients or radios and the rest of the network. The reference implementation is called **roost**, by analogy to where a bird spends the night: wherever it happens to be, not a fixed home. A callsign may have several nodes at once.
- **Home node** for a callsign means any node that has heard it and is holding state for it. It does not imply registration or permanence.
- **Public node** is a node with a reachable address that also relays for nodes behind NAT and serves internet-only clients.
- **Clock-in** is the informal name for a delivery receipt (RCPT), after the timing clock that records a racing pigeon's return.

Additional terms used here:

- **Origin** of a piece of gossip is the node that observed it directly. Gossip is replicated but never re-originated.
- **Custody** of a message is the responsibility to deliver it or report its expiry. Exactly one node holds custody at a time, except transiently during transfer.

## 1. Purpose

This document defines how nodes find each other, authenticate, share the state needed for routing (presence, subscriptions, pending mail), move messages between themselves, and recover history. Clients and radios never speak this protocol; they speak M17_inet to a node, as described in the architecture notes.

## 2. Conventions

- All multi-byte integers are big-endian.
- Timestamps are unsigned 32-bit seconds since the Unix epoch.
- "Envelope bytes" means a complete MSG or RCPT payload as defined in the Message Envelope specification, including its type byte.
- Message payloads in this protocol are encoded as CBOR maps with small unsigned integer keys (see §3.3). Field tables in this document list the key, the CBOR type, and the meaning. Unknown keys must be ignored; absent optional keys take their documented default.

## 3. Transport

### 3.1 TCP and TLS

Nodes communicate over TCP. Every connection is wrapped in TLS 1.3 with mutual authentication: both sides present a certificate, and both verify the other's as described in §4. Plaintext node connections are not permitted in any version of this protocol.

The default listening port is **17700** (provisional). Public nodes listen; nodes behind NAT do not need to.

TLS provides authentication and integrity on the link. It does not provide message confidentiality: message content is plaintext at every node and on RF, as required for amateur traffic.

### 3.2 Framing

Each protocol message is sent as:

| Size | Field   | Description |
|-----:|---------|-------------|
| 4    | Length  | Payload length in bytes, at most 1 048 576 |
| 1    | Type    | Message type (§3.4) |
| var  | Payload | CBOR-encoded body |

Length counts the Type byte and the payload. A frame exceeding the maximum is a protocol error; the receiver closes the connection.

### 3.3 Encoding

Payloads are CBOR (RFC 8949) maps with unsigned integer keys. Callsigns and room addresses are encoded as 6-byte byte strings holding the 48-bit M17 address. Node IDs are 32-byte byte strings (§4.1). Envelope bytes are byte strings.

CBOR is chosen over a hand-rolled binary format because this protocol will evolve far more than the envelope, and over Protocol Buffers to avoid a code-generation step and dependency. See Open Questions.

### 3.4 Message Types

| Value  | Name         | Direction | Section |
|-------:|--------------|-----------|---------|
| `0x01` | HELLO        | both      | §5.2 |
| `0x02` | PEERS        | both      | §5.4 |
| `0x03` | PING         | both      | §5.5 |
| `0x04` | PONG         | both      | §5.5 |
| `0x10` | DIGEST       | both      | §6.4 |
| `0x11` | DELTA_REQ    | both      | §6.4 |
| `0x12` | UPDATE       | both      | §6.4 |
| `0x20` | DELIVER      | both      | §7.2 |
| `0x21` | ACK          | both      | §7.3 |
| `0x30` | BACKLOG_REQ  | both      | §9 |
| `0x31` | BACKLOG      | both      | §9 |
| `0x7F` | ERROR        | both      | §3.5 |

### 3.5 Errors

An ERROR message carries a code and a text reason. It is informational; a node that considers the condition fatal closes the connection after sending it.

| Key | Type   | Meaning |
|----:|--------|---------|
| 1   | uint   | Code: 1 = protocol error, 2 = unsupported version, 3 = identity mismatch, 4 = refused, 5 = rate limited |
| 2   | text   | Reason |

## 4. Identity and Trust

### 4.1 Node Identity

Each node has a long-lived ECDSA secp256r1 key pair, the same curve the Message Envelope uses for signatures. The node's **node ID** is the SHA-256 of the DER-encoded SubjectPublicKeyInfo of its public key. Nodes present a self-signed X.509 certificate for that key in the TLS handshake. Certificate expiry is ignored; the key is the identity.

Each node also has a **node callsign**: the amateur callsign of the operator, optionally with a suffix (e.g. `N1ADJ-R`). The callsign is a claim; the key is the identity. Two nodes may claim the same callsign (an operator running several), and they are distinct nodes.

Nodes that are single-user clients (a client with an embedded node and no RF side) use the user's callsign as the node callsign and may use the same key for node identity and message signatures.

### 4.2 Verification and Pinning

On connection, each side checks that the peer's certificate is self-signed with the key it contains, and computes the node ID. Trust is on first use: the first time a node ID is seen, it is recorded with its claimed callsign; subsequently, a connection presenting that node ID with a different callsign is refused with ERROR code 3. Operators may pre-configure trusted node IDs and may mark node IDs as blocked.

This gives every claim in the protocol an accountable origin. It does not, in this version, prevent a node from lying; it ensures that lies are attributable to a key, which is the foundation for any later reputation or allow-list mechanism.

### 4.3 What Is and Is Not Trusted

- **Message integrity** comes from envelope signatures, when present. A node cannot forge or alter a signed message.
- **Routing trust** comes from node identity. Presence and subscription claims are only as trustworthy as the node that originated them and the way it observed them (§6.2, `heard_via`).
- **Confidentiality** is not provided by anything in this system.

## 5. Peering

### 5.1 Topology

Every node maintains outbound connections to a small number of peers (default 3, configurable). Public nodes additionally accept inbound connections, up to a configured limit. A NATed node's peers are necessarily public nodes; a public node's peers may be anything.

The resulting graph is unstructured. Gossip (§6) floods over it with deduplication, and delivery (§7) routes over it using the reachability information gossip provides. No spanning tree is maintained.

### 5.2 HELLO

Sent by both sides immediately after the TLS handshake. No other message may precede it.

| Key | Type          | Meaning |
|----:|---------------|---------|
| 1   | uint          | Protocol version; this document is version 0 |
| 2   | bytes(6)      | Node callsign |
| 3   | uint          | Capability flags: bit 0 = accepts inbound (public), bit 1 = relays for others, bit 2 = serves M17_inet clients |
| 4   | array of text | Listen addresses (`host:port`), present only if public |
| 5   | bytes(32)     | Epoch: random value chosen at node start (§6.3) |
| 6   | uint          | Current time at sender |
| 7   | text          | Software name and version (informational) |

A node receiving a HELLO with an unsupported version replies ERROR code 2 and closes. A node whose clock differs from the peer's by more than a configured tolerance (default 5 minutes) should log it; timestamps in gossip are origin-relative and the protocol tolerates modest skew.

### 5.3 Bootstrap

A node with no known peers obtains seeds from, in order: its configuration file; DNS TXT records at a configured seed name (format: `v=pigeon0; addr=host:port; id=<hex node ID>`, one record per seed); and any PEERS messages it has cached from previous runs. Seed lists are a convenience, not an authority: a seed is just a public node somebody wrote down.

### 5.4 PEERS

A list of public nodes the sender knows about, sent after HELLO and thereafter whenever the sender's view changes substantially, at most once per minute.

| Key | Type  | Meaning |
|----:|-------|---------|
| 1   | array | Entries, each a map: 1 = node ID (bytes), 2 = callsign (bytes), 3 = addresses (array of text), 4 = last seen (timestamp) |

Receivers merge entries into their peer table and may use them to open additional connections. Entries older than 7 days are dropped.

### 5.5 Liveness

A node sends PING if it has sent nothing for 60 seconds; the peer answers PONG. A node that has received nothing for 180 seconds closes the connection. Both messages have an empty payload. TCP keepalive may be enabled in addition but is not relied on.

A node whose outbound peer count falls below its target reconnects, preferring peers from its table with the most recent `last seen`.

## 6. Gossip

### 6.1 Model

Each node is the **origin** of exactly one **table**: the set of things it has directly observed. Every node also holds a replica of every other node's table, as best it has received it. Tables are replicated by flooding updates and reconciled by periodic digest exchange. Nothing is ever re-originated: a node forwarding another node's update leaves the origin, epoch, and sequence numbers untouched.

An origin's table has three kinds of entries:

- **Presence entries**, keyed by callsign (§6.2)
- **Pending entries**, keyed by destination callsign (§6.2)
- **Node entry**, exactly one, describing the origin itself (§6.2)

Room subscriptions are carried inside presence entries; the set of rooms a node carries is derived by receivers, not gossiped separately. This resolves Rooms open question 1.

### 6.2 Entry Types

Every entry carries:

| Key | Type      | Meaning |
|----:|-----------|---------|
| 1   | uint      | Kind: 1 = presence, 2 = pending, 3 = node |
| 2   | uint      | Sequence: the origin's sequence number when this entry was last changed (§6.3) |
| 3   | timestamp | Expires: after this time receivers discard the entry |
| 4   | bool      | Tombstone: true if the entry is a deletion; retained until Expires, then dropped |

**Presence** (kind 1):

| Key | Type     | Meaning |
|----:|----------|---------|
| 10  | bytes(6) | Callsign |
| 11  | uint     | Heard via: 1 = RF, 2 = directly connected client (LAN), 3 = internet client (M17_inet over the internet) |
| 12  | timestamp| Last heard |
| 13  | array    | Rooms: each a map of 1 = room address (bytes(6)), 2 = refreshed (timestamp), 3 = left (bool) |

Presence Expires defaults to Last heard + 30 days, matching subscription expiry in the Rooms specification. A node updates a callsign's presence entry when it first hears the callsign, when its rooms change, and otherwise no more than once per 5 minutes per callsign, to bound gossip volume from chatty stations.

**Pending** (kind 2):

| Key | Type      | Meaning |
|----:|-----------|---------|
| 20  | bytes(6)  | Destination callsign |
| 21  | uint      | Count of messages held |
| 22  | timestamp | Latest expiry among them |

Published by any node holding custody of undelivered unicast messages for a callsign. Removed (tombstoned) when the count reaches zero.

**Node** (kind 3):

| Key | Type          | Meaning |
|----:|---------------|---------|
| 30  | bytes(6)      | Node callsign |
| 31  | uint          | Capability flags, as in HELLO |
| 32  | array of text | Listen addresses, if public |
| 33  | array of bytes(32) | Via: node IDs of the public nodes this node is currently connected to, if it is not itself public |

The Node entry is what makes a NATed node reachable: any node can route to it through one of its Via nodes (§7.4). It is refreshed whenever the Via set changes.

### 6.3 Sequence and Epoch

Each origin keeps a 64-bit sequence counter, incremented for every change to its table. Each entry records the sequence at which it last changed. An origin's **epoch** is a random 32-byte value chosen at process start and sent in HELLO and in every UPDATE. When a receiver sees an origin with a new epoch, it discards everything it held for that origin: the node restarted and its counter reset.

### 6.4 Replication

**UPDATE** carries one or more entries from a single origin:

| Key | Type      | Meaning |
|----:|-----------|---------|
| 1   | bytes(32) | Origin node ID |
| 2   | bytes(32) | Origin epoch |
| 3   | array     | Entries (§6.2) |

A node sends UPDATE to all peers when its own table changes (batched, at most once per second). A node receiving an UPDATE applies each entry if its sequence is newer than what it holds for that (origin, key), and forwards the UPDATE, minus entries it already had, to all peers except the one it came from. This is flooding with dedup; an entry crosses any given link at most once in each direction.

**DIGEST** is sent to each peer every 60 seconds and lists, for every origin the sender knows, the highest sequence it holds:

| Key | Type  | Meaning |
|----:|-------|---------|
| 1   | array | Each a map: 1 = origin node ID, 2 = epoch, 3 = highest sequence |

A node receiving a DIGEST compares it with its own state. For each origin where the peer is behind, it sends UPDATE with the entries the peer lacks (those with sequence greater than the peer's). For each origin where the peer is ahead, or unknown to the receiver, it sends **DELTA_REQ**:

| Key | Type      | Meaning |
|----:|-----------|---------|
| 1   | bytes(32) | Origin node ID |
| 2   | uint      | Highest sequence held (0 if none) |

and the peer answers with UPDATE. This anti-entropy pass catches anything flooding missed and bootstraps a new node's view.

### 6.5 Expiry and Size

Receivers drop entries past Expires and tombstones past Expires. An origin that has been silent (no UPDATE or DIGEST mention from any peer) for 7 days is dropped entirely. Presence tables are the dominant cost: at roughly 60 bytes per callsign, ten thousand active callsigns is under a megabyte replicated at every node, which is acceptable for this version. Summarization for larger networks is a later concern (see Open Questions).

## 7. Delivery

### 7.1 Custody

A message enters the system at the sender's home node, which takes **custody**: it stores the message, publishes a Pending entry, and, if RCPT_REQ is set, issues QUEUED. The custodian's job is to get the message to a node where the destination is present, hand custody over, and then forget it.

Custody transfers only on an ACK with result ACCEPTED (§7.3). Until then the custodian retains the message and retries. A node never holds custody of a message it did not either receive from a client or accept via ACK.

### 7.2 DELIVER

| Key | Type      | Meaning |
|----:|-----------|---------|
| 1   | bytes(32) | Target node ID |
| 2   | bytes(32) | Source node ID (the custodian, or for room messages the originating node) |
| 3   | uint      | Hops remaining; decremented at each forward, dropped at zero (initial 8) |
| 4   | bytes     | Envelope bytes |
| 5   | timestamp | Received-at: when the custodian first received the message, used for expiry when the envelope timestamp is unknown |
| 6   | bool      | Custody offered: true for unicast delivery to a home node, false for room fan-out and receipts |

Routing of DELIVER is described in §7.4. The envelope is opaque to intermediate nodes; only the target node parses it.

### 7.3 ACK

Sent by the target node back to the source node, routed the same way.

| Key | Type      | Meaning |
|----:|-----------|---------|
| 1   | bytes(32) | Target node ID (the original source) |
| 2   | bytes(32) | Source node ID (the responder) |
| 3   | uint      | Hops remaining |
| 4   | bytes(8)  | Message ID |
| 5   | uint      | Result: 1 = ACCEPTED, 2 = DUPLICATE, 3 = NOT_HOMED, 4 = REFUSED, 5 = EXPIRED |
| 6   | text      | Reason (optional) |

- ACCEPTED: the responder has stored the message and, if custody was offered, now holds custody. For room messages it means the message was stored for local subscribers.
- DUPLICATE: the responder already had this message ID. Treated as ACCEPTED by the sender.
- NOT_HOMED: the responder does not currently hold state for the destination callsign. The sender's presence view was stale; it waits for a fresher entry.
- REFUSED: policy refusal (blocked callsign, room not carried, rate limit). Custody stays with the sender; if no other node can accept, the message expires normally.
- EXPIRED: the responder computed that the message is past its expiry.

### 7.4 Routing

Every DELIVER and ACK carries a target node ID. A node receiving one it is not the target of forwards it:

1. If the target is a direct peer, to that peer.
2. Else if the target's Node entry lists Via nodes and any is a direct peer, to that peer.
3. Else if any Via node is known, to a public-node peer (public nodes are well connected and will reach the Via node in one or two more hops).
4. Else, drop and, for DELIVER, return an ACK with REFUSED and reason "unreachable" if a path back to the source exists.

Hops remaining bounds all of this. There is no routing table beyond the gossip state; the Node entries *are* the routing table.

### 7.5 Unicast Delivery Procedure

For each message in custody with destination X, the custodian:

1. Consults its replica of all origins' Presence entries for X. Candidate nodes are those whose entry for X is not expired and whose Last heard is within the **active window** (default 24 hours), ordered by Last heard, most recent first.
2. Sends DELIVER with custody offered to each candidate (all of them, not just the first: X may have several radios).
3. On the first ACCEPTED, drops custody, removes X from its Pending entry, and stops retrying. Later ACCEPTEDs from other candidates are harmless; each of those nodes delivers to its own client and the client dedups by message ID.
4. If no candidates exist, or all respond NOT_HOMED, keeps custody and waits. Any Presence update for X restarts the procedure.
5. Retries an un-ACKed DELIVER after 60 seconds, then with exponential backoff to a 1 hour ceiling, until the message expires.
6. On expiry, discards the message and, if RCPT_REQ was set, issues EXPIRED with Last heard set from the most recent presence it knows.

A node that accepts custody delivers to its local client or radio, issues TRANSMITTED if appropriate, and holds the message for the destination's other local clients until the message expires. Because every home node of X will end up with a copy, X's history is available wherever X shows up.

### 7.6 Receipts

RCPT envelopes are delivered exactly like MSG but without custody: the originating node sends DELIVER (custody false) to each candidate node for the receipt's destination, and does not retry beyond a few attempts. Receipts are best-effort by design.

### 7.7 Room Delivery

A room message entering at node A is stored for A's local subscribers, transmitted once on RF, and then sent by A as DELIVER (custody false) to every node whose derived room set includes the room. Each receiving node stores it for its local subscribers and transmits once. A retries un-ACKed room DELIVERs for up to one hour, then stops. Custody is not transferred; the message's continued existence is guaranteed by whichever subscriber nodes stored it.

This is direct fan-out from the origin. It is O(nodes carrying the room) per message, which is fine for the sizes this version targets; a subscription tree would be needed for rooms with thousands of nodes and is noted under Open Questions.

## 8. Node Behaviour Toward Clients

The client side is M17_inet and is not part of this protocol, but three behaviours are required for the protocol to work:

- **Presence.** A node originates a Presence entry for every callsign it hears on RF or from a connected client, with `heard_via` set truthfully.
- **Proxying.** A node forwards stream frames and packet types it does not handle to the client's chosen upstream reflector unchanged. It intercepts MSG, RCPT, ROOM, and SMS.
- **Dedup toward clients.** A node never sends the same message ID to the same client twice, so that a client homed at several nodes, or a message that arrives by several paths, produces one displayed message.

## 9. Backlog

Used when a node gains a subscriber for a room it holds no history for (Rooms §7.3).

**BACKLOG_REQ**:

| Key | Type      | Meaning |
|----:|-----------|---------|
| 1   | bytes(32) | Target node ID |
| 2   | bytes(32) | Source node ID |
| 3   | uint      | Hops remaining |
| 4   | bytes(6)  | Room address |
| 5   | timestamp | Since: only messages with origin timestamp after this |
| 6   | uint      | Maximum number of messages (at most 200) |

**BACKLOG**:

| Key | Type           | Meaning |
|----:|----------------|---------|
| 1   | bytes(32)      | Target node ID |
| 2   | bytes(32)      | Source node ID |
| 3   | uint           | Hops remaining |
| 4   | bytes(6)       | Room address |
| 5   | array of bytes | Envelope bytes, oldest first |
| 6   | bool           | Truncated: more exist than were sent |

A node answers BACKLOG_REQ from any node, subject to rate limiting, and sends only messages still within their expiry. The requester dedups by message ID. Backlog is delivered to IP clients; whether it is transmitted on RF is a node configuration choice, default off (Rooms open question 3).

## 10. Defaults

| Parameter | Default |
|-----------|---------|
| Listen port | 17700 (provisional) |
| Outbound peers | 3 |
| Inbound peers (public) | 64 |
| PING interval / dead timer | 60 s / 180 s |
| DIGEST interval | 60 s |
| UPDATE batching | 1 s |
| Presence update rate | 1 per 5 min per callsign |
| Presence / subscription expiry | 30 days after last heard |
| Active window for delivery | 24 h |
| DELIVER retry | 60 s, backoff to 1 h |
| Room DELIVER retry limit | 1 h |
| Hops | 8 |
| Silent-origin drop | 7 days |
| Max frame | 1 MiB |
| Clock tolerance | 5 min |

## 11. Open Questions

1. **Encoding.** CBOR with integer keys is proposed. Protocol Buffers would give a schema and generated code at the cost of a build dependency. Worth deciding before the first line of Go.
2. **Room fan-out at scale.** Direct fan-out from the origin is simple and correct but linear in the number of nodes carrying a room. A tree (or having public nodes fan out on behalf of NATed origins) is the obvious next step; nothing here precludes it.
3. **Presence summarization.** Full presence replication is fine to tens of thousands of callsigns. Beyond that, nodes would gossip per-node Bloom filters and fetch detail on demand. The DELTA_REQ mechanism is the natural hook.
4. **Multiple Via nodes.** A NATed node should connect to at least two public nodes so that one going down does not make it unreachable; is that a requirement or a recommendation?
5. **Abuse.** Rate limits per origin node and per callsign are implied but not specified. A node can already block node IDs and callsigns; is per-node reputation wanted in this version, or is "block it and tell the operator" enough?
6. **Key rotation.** A node that loses its key becomes a new node. Is a signed "successor" statement worth having, or is starting fresh acceptable?
7. **DNS seed format and names.** Proposed above; needs a real domain and a decision about who maintains seeds.

## 12. Resolved

- **Transport:** TCP with mandatory mutual TLS 1.3, self-signed certificates, trust-on-first-use pinning by node ID.
- **Node identity:** ECDSA secp256r1 key, node ID = SHA-256 of the public key; node callsign is a claim, the key is the identity.
- **Trust model:** message integrity from envelope signatures; routing trust from node identity, qualified by `heard_via`; no confidentiality.
- **Gossip:** per-origin tables with sequence numbers and epochs, flooded with dedup and reconciled by digest.
- **Subscriptions** travel inside presence entries; per-node room sets are derived.
- **Delivery:** custody transfer for unicast; best-effort fan-out for rooms and receipts.
- **Reachability:** NATed nodes are routed to via the public nodes they are connected to, advertised in their Node entry.
