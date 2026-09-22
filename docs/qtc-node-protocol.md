# QTC: Node Protocol

*Part of QTC: An M17 Messaging System*

**Status:** Draft 0.2 — for discussion. Supersedes draft 0.1 (custody-based).
**Scope:** Node identity and peering, presence, mailbox records, the storage protocol, delivery, and rooms transport
**Depends on:** QTC: Architecture Overview; QTC: Message Envelope; QTC: Rooms

## 1. Substrate

Nodes are go-libp2p hosts. This document assumes, and does not respecify:

- **Transport:** TCP with libp2p TLS. Plaintext connections are not permitted.
- **Identity:** the libp2p peer ID derived from the node's ECDSA secp256r1 key. "Node ID" below means this peer ID.
- **Discovery:** configured bootstrap peers, DNS bootstrap, and the Kademlia DHT.
- **NAT:** AutoNAT for reachability detection, circuit relay v2 through public stations.
- **Pubsub:** gossipsub, with message IDs derived from content so duplicates are dropped.

QTC defines: what nodes advertise, the topics and what goes on them, one DHT record type, and one stream protocol.

Protocol IDs and topic names carry a version segment; this document defines version `0`.

### 1.1 Encoding

All QTC payloads (pubsub messages, DHT records, and store-protocol messages) are JSON objects. Field types used below:

| Type | JSON representation |
|---|---|
| address | string: the canonical text form of an M17 address, up to 9 characters (`"N1ADJ  H"`, `"N1ADJ"`), or `#` followed by the canonical room name for a room address (`"#MAINE"`). The `#` here is the protocol's text form for the Extended range and is unrelated to the rule that users never type `#` in room names (Rooms §3.1). |
| nodeid | string: the libp2p peer ID in its standard text encoding |
| msgid | string: 16 lowercase hex characters (the 8-byte message ID) |
| env | string: base64 (standard, padded) of the envelope bytes, unmodified |
| timestamp | integer: seconds since the Unix epoch |
| bytes | string: base64 |

Envelopes are never converted to JSON objects; they are carried and stored as the exact bytes that appeared on the air, so that message IDs and signatures survive every hop. Implementations may include derived fields (`id`, `src`, `dst`) alongside `env` for readability, but receivers must recompute them from the bytes and never trust them.

Unknown fields must be ignored. JSON is chosen over a binary encoding for debuggability and ease of second implementations; message volume between nodes is small enough that size is not a concern.

## 2. Node Advertisement

Each node publishes, in its presence messages (§3), a **node card**. libp2p identify carries only an agent string and a protocol list, so the card travels in presence alone; the identify agent string is set to the card's `software` value.

| Key | Type | Meaning |
|---|---|---|
| callsign | address | Node callsign, used whole |
| caps | integer | Capability flags: `public` 1, `relay` 2, `mailbox` 4, `clients` 8 |
| software | string | Name and version (informational) |

## 3. Presence

Topic: `/qtc/0/presence`

Every node publishes a presence message on change and at least every 5 minutes, whether or not it has any active callsigns, since presence is how other nodes learn its card (a mailbox node with no local users must still be discoverable):

| Key | Type | Meaning |
|---|---|---|
| node | nodeid | Publishing node ID. Informational: receivers take the publisher's identity from the gossipsub message signature and ignore a message whose `node` does not match the signer |
| card | map | Node card (§2) |
| time | timestamp | Publisher's current time |
| heard | array | Entries, each: `device` address (full, with suffix), `via` integer (1 RF, 2 local client, 3 internet client), `last` timestamp |

Receivers keep, per (node, device), the latest entry, and drop entries whose `last` is older than 30 days or whose publishing node has been silent for 7 days. A node's *stations* for a base callsign are the nodes with an unexpired entry for any of its devices. A callsign is *active* at a node if `last` is within the active window (24 hours).

To bound traffic, a node includes a device in `heard` when it is first heard, when its `via` changes, and otherwise at most once per 5 minutes per device.

## 4. Mailbox Records

DHT key: `/qtc/0/mailbox/<BASE>` where `<BASE>` is the base callsign in canonical text form.

| Key | Type | Meaning |
|---|---|---|
| callsign | address | Base callsign |
| version | integer | Monotonically increasing per record |
| home station | nodeid | Node ID of the home station |
| members | array of nodeid | Node IDs of mailbox nodes; target size `k` |
| k | integer | Target size (default 2) |
| policy | integer | Flags: `split_by_suffix` 1, `provisional` 2 |
| updated | timestamp | |
| writer | nodeid | Node ID that wrote this version |
| writer_key | bytes | The writer's public key as SubjectPublicKeyInfo DER. Needed because an ECDSA peer ID is a hash of the key, not the key itself |
| sig | bytes | Writer's ECDSA signature, raw `r ‖ s` (64 bytes), over the canonical field concatenation below |

**Signature input.** JSON has no canonical form, so the signature is computed over a fixed byte string rather than the serialized record: the 6-byte encoded `callsign`, `version` as 8 bytes big-endian, the raw bytes of `home station`, then each member's raw bytes in the order listed, `k` as 1 byte, `policy` as 2 bytes big-endian, `updated` as 4 bytes big-endian, and the raw bytes of `writer`. Readers rebuild this from the parsed fields and verify.

**Validation** (applied by the DHT validator and by every reader): `writer_key` hashes to `writer`; `sig` verifies against `writer_key`; `version` is greater than any previously seen version for this callsign. Ties and lower versions are rejected.

**Authority** (a convention nodes must follow, not enforceable by the validator): a node may write a record only if it is the record's `home station`, or there is no record, or the current `home station` has been silent in presence for the takeover period (default 7 days), or it is handing off a record it created on a sender's behalf (§7.3). Readers should prefer a record whose `writer` equals its `home station` when versions conflict in the DHT's eventual consistency.

**Creation.** The first node to home a callsign that has no record creates one with itself as `home station` and `k` mailbox-capable public stations as `members`. It prefers public stations it is already connected to (typically its circuit relays), since those are known to be reachable from the user's location; if it has fewer than `k` of those, it fills the set with mailbox-capable nodes found via the DHT.

Mailbox records are also announced on `/qtc/0/mailbox-records` when written, so nodes that already care about the callsign learn of changes without polling the DHT.

## 5. Storage Protocol

Stream protocol ID: `/qtc/0/store`. Opened by any node to a node advertising `mailbox`. Messages are JSON objects with a `type` field, one per line (JSON Lines); maximum 1 MiB per message.

| Type | Direction | Fields | Meaning |
|---|---|---|---|
| PUT | → mailbox | `callsign` address (base), `env` env | Store an envelope under a base callsign |
| PUT_OK | ← mailbox | `id` msgid | Stored (or already present) |
| PUT_ERR | ← mailbox | `id` msgid, `code` integer, `reason` string | 1 refused (including a live-only MSG with TTL 0, which must not be stored), 2 quota, 3 expired, 4 invalid |
| QUERY | → mailbox | `callsign` address, `since` timestamp (inclusive), `limit` integer (default 100, maximum 1000; the mailbox clamps), `types` array of integer (envelope types; default MSG and RCPT, so a sweep that wants subscription state must ask for ROOM) | Fetch envelopes stored since `since`, by received-at time |
| RESULT | ← mailbox | `envs` array of env, `next` timestamp or null | `next` non-null means more exist; query again from it. `next` is the received-at time of the last envelope returned, and a RESULT always includes every envelope sharing that second, so a page may exceed `limit` slightly and pages may overlap; callers union by ID |
| WATCH | → mailbox | `callsigns` array of address | Push new puts for these callsigns on this stream until it closes |
| EVENT | ← mailbox | `callsign` address, `env` env | A newly stored envelope for a watched callsign |
| UNWATCH | → mailbox | `callsigns` array of address | |

Mailbox nodes store an envelope until its expiry (Message Envelope §4.4), computed from the envelope timestamp or, if unknown, the time of the first PUT. Storage is keyed by base callsign and message ID; a PUT of an existing ID is PUT_OK and a no-op. RCPT and ROOM envelopes have no message ID of their own (a receipt's Message ID field names the original message, and several receipts for one message must coexist; Rooms §5.3); for storage and PUT_OK their ID is the first 8 bytes of SHA-256 over the envelope bytes as stored, i.e. for ROOM after any timestamp substitution. Mailbox nodes never contact each other.

Replies are sent in request order on the stream; EVENT messages may appear between any two messages. A client should keep one request in flight per stream. A message with an unknown `type` is ignored and logged, so types can be added without breaking older mailboxes.

Retention: a MSG is kept until its expiry as above. RCPT and ROOM envelopes carry no TTL; a RCPT is kept for the node's default TTL from the time of the first PUT, and a ROOM envelope for the subscription expiry (Rooms §4.5, default 30 days) from the time of the first PUT.

A mailbox node accepts PUT for any callsign, subject to per-writer and per-callsign quotas (unspecified; see Open Questions).

## 6. Sending

When a station accepts a message from a client or radio (a native MSG, or an SMS it wraps per Message Envelope §6), it:

1. Normalizes source and destination to base callsigns, retaining the full source as the device.
2. Reads the destination's mailbox record. If none exists, creates one (§7.3).
3. PUTs the envelope to every member of the destination's mailbox **and** every member of the sender's own mailbox, so the sender's record includes what they sent.
4. Keeps its own copy and retries any member that has not returned PUT_OK, with backoff, until the message expires. A member that is unreachable long enough to be replaced (§8.2) is dropped from the retry set.
5. If RCPT_REQ is set, issues QUEUED once at least one member has accepted.

Receipts are envelopes and are sent the same way, to the original sender's mailbox, without retry beyond a few attempts.

## 7. Homing

A station *homes* a base callsign from the moment it hears any of its devices until presence for all of them expires. While homing:

### 7.1 On first hearing

1. Publish presence for the device.
2. Read the mailbox record. If none, create one (§4) with this node as home station.
3. Open `/qtc/0/store` streams to each member and WATCH the callsign.
4. Run a sweep (§7.2).
5. Replay to the local device any message from the sweep whose origin timestamp is within the replay window (default 3 hours) and for which no DELIVERED receipt exists in the mailbox. Native clients dedup by message ID; legacy radios may see repeats.

### 7.2 Sweep

QUERY every member since the station's last sweep time for this callsign (or the full retention window if never), union the results by message ID, and PUT to each member whatever it lacks. This is the only reconciliation mechanism between mailbox nodes. Run on first hearing and thereafter at a low rate (default hourly) while the callsign is active.

### 7.3 Records created by senders

A station that creates a record for a callsign it has never heard (because a local user sent to it) sets itself as home station but marks the record with `policy` bit `provisional` (2). The first station to actually hear the callsign takes over the record with a new version and clears the bit; the creating node must accept this regardless of the takeover period.

### 7.4 Delivery

On EVENT, or on a message found by a sweep, a station delivers once per local device that the destination addresses (all devices if the destination has no suffix; only the matching device if it has one), issuing TRANSMITTED with that device's `last` heard time if RCPT_REQ was set. It never delivers the same message ID to the same device twice.

## 8. Home stations and Repair

### 8.1 Home station

The home station is the node named in the mailbox record. It alone writes record changes in normal operation. Other stations read, deliver, sweep, and put; if they detect a condition requiring a record change, they wait for the home station to act, unless the takeover condition holds (§4).

### 8.2 Repair

On each sweep, the home station checks every member. A member is *failed* if it has been unreachable across at least three sweeps **and** its node has been silent in presence for the failure period (default 24 hours), so that a local network problem does not churn the record. For each failed member, the home station recruits a mailbox-capable public station not already in the set, PUTs the callsign's entire retained history to it (from the sweep union), removes the failed member, and writes a new record version.

If the set is larger than `k` because of concurrent repair, the home station may trim it at the next repair, preferring to drop the member least recently reachable.

### 8.3 Migration

A home station may replace a member for reasons other than failure (moving to a better-placed public station) at most once per day, using the same recruit-copy-write sequence. Policy for choosing placement is local to the home station.

### 8.4 Takeover

If the home station's node has been silent in presence for the takeover period, any station currently homing the callsign may write a record naming itself home station. Concurrent takeovers resolve by version and the DHT's ordering; the losing node re-reads and defers. Takeover is expected to be rare and is logged with the old and new node IDs.

## 9. Rooms Transport

Topic: `/qtc/0/room/<NAME>` where `<NAME>` is the canonical room name (Rooms §3.1).

A station subscribes to a room's topic while it has any local subscriber to the room, including auto-subscribed callsigns for its node-callsign room. A room message is published to the topic; every subscribed station stores it for its local subscribers and transmits once. Gossipsub's mesh does fan-out and dedup.

**Room archives.** Public stations with `mailbox` that subscribe to a room also store its messages for their TTL, keyed by room address, and answer QUERY for the room address exactly as for a callsign. A station needing backlog (Rooms §7.3) queries any such node; gossipsub's peer list for the topic identifies candidates. Whether backlog is transmitted on RF is a station setting, default off.

Per-callsign subscription lists (needed so a callsign's rooms follow it between stations) are carried in the mailbox record's storage as ROOM envelopes: a station that processes a JOIN or LEAVE PUTs a ROOM envelope to the callsign's mailbox, and a station newly homing the callsign reads the latest ROOM state from its sweep. This resolves Rooms open question 1 without a separate table.

## 10. Node Behaviour Toward Clients (the client face)

The client side is M17_inet and outside this protocol; this section records how a node behaves on it.

A node presents itself as one M17_inet reflector with its own name (for example `M17-QTC`) on one UDP port. Gateways and clients link to it by choosing that name, exactly as they would any reflector, so using QTC is an explicit choice made at the gateway. The node is configured with a map from module letter to an upstream reflector and module and a **mode**, `native` or `qtc`. A CONN or LSTN for an unmapped module is answered with NACK.

A client's link is to the node: the node answers its CONN or LSTN with ACKN itself once the module is mapped. It then opens an upstream connection to the mapped reflector, resending the CONN until the reflector accepts (reflectors drop or NACK a CONN while a stale link for the same callsign is timing out), and forwards PING, PONG, DISC, and stream frames unchanged in both directions. An upstream NACK or outage is logged and retried, never propagated to the client. Voice is never touched.

**Native mode** is a plain proxy: every packet passes through unchanged in both directions, no presence is published for devices heard on the module, and nothing is delivered to them. **QTC mode** is QTC-only for messaging:

- MSG, RCPT, and ROOM packets from the client are taken into the node and never forwarded. (These are always taken in, whatever the mode, since no reflector understands them.)
- SMS from the client is wrapped per Message Envelope §6 and sent through QTC; it is not forwarded upstream. An SMS addressed to the node's callsign is a room command if its text begins with `/`, a message to a named room if it begins with `#NAME `, and otherwise a message to the node's local room (Rooms §6). When matching its own callsign the node ignores the number of spaces: the module convention pads `N1ADJ  M` so the letter sits in the ninth position, but radio UIs collapse the padding and send `N1ADJ M`.
- Messaging packets (SMS, MSG, RCPT, ROOM) arriving from the upstream reflector are dropped; everything else passes through.
- The LSF source of every stream frame and packet from the client is published in presence, `via` RF when the client is a gateway (a configured set of addresses, by default the local network) and `via` internet client otherwise.
- A MSG for a device heard on the module is delivered as SMS (Message Envelope §6) in an M17_inet packet to the client that heard it; a room message is addressed to the device with `#NAME ` prefixed to the text (Rooms §6). Receipts are dropped for legacy clients.

In all modes the node never sends the same message ID to the same device twice.

A consequence to be aware of: a user on a qtc-mode module can exchange messages only with other QTC users. Their SMS never reaches the reflector and reflector SMS never reaches them. That is the intended trade for unambiguous behaviour; the reflector remains available by linking to it directly.

## 11. Defaults

| Parameter | Default |
|---|---|
| Mailbox target size `k` | 2 |
| Presence publish interval | 5 min |
| Presence expiry | 30 days after last heard |
| Silent node expiry | 7 days |
| Active window | 24 h |
| Replay window | 3 h |
| Sweep interval while active | 1 h |
| Member failure period | 24 h silent and 3 failed sweeps |
| Home station takeover period | 7 days silent |
| Migration rate | 1 per day |
| Max store message | 1 MiB |
| Message retention | envelope TTL (default 7 days) |

## 12. Open Questions

1. **Presence establishment on the radio side.** Deliberately unresolved; see Architecture §10.
2. **Presence topic scale.** One gossipsub topic for all presence is fine to a few thousand nodes. Beyond that, regional topics or per-callsign-prefix sharding; nothing here precludes it.
3. **DHT validator limits.** The validator can enforce signature and version but not the home station convention. A malicious node can still overwrite a record; readers preferring `writer == home station` mitigates but does not prevent it. User-signed records are the real fix.
4. **Quotas.** Per-writer and per-callsign PUT limits on mailbox nodes, and limits on record creation, are needed before public deployment and are not specified.
5. **Thin clients.** Whether phones and browsers join as libp2p peers over WebSocket or public stations expose `/qtc/0/store` over plain WebSocket. After the spike.
6. **Weight.** go-libp2p on a first-generation Pi Zero. The spike measures it.

## 13. Resolved

- **Encoding:** JSON, with envelopes as base64 blobs and the mailbox-record signature over a fixed field concatenation rather than the serialization.
- **Substrate:** go-libp2p, replacing the hand-rolled transport, peering, gossip, and routing of draft 0.1.
- **Delivery model:** declared mailboxes (fixed set, size `k`) instead of custody transfer; senders write to all members and retry.
- **Reconciliation:** homing-node sweeps; mailbox nodes never talk to each other.
- **Authority:** the home station, with silent-period takeover, until user keys exist.
- **Identity:** base callsign keys everything; suffix is the device.
- **Rooms:** gossipsub topics; subscription state stored as ROOM envelopes in the mailbox.
