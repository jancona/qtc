# QTC: Native Clients

*Part of QTC: An M17 Messaging System*

**Status:** Draft 0.1 — for discussion. The other specifications and the fixtures reflect it.
**Scope:** The QTC packet-mode payload, and how native devices exchange it with their node reliably
**Depends on:** QTC: Architecture Overview; QTC: Message Envelope; QTC: Rooms; QTC: Node Protocol §7, §10

## Terminology

This document is part of **QTC: An M17 Messaging System**. Throughout the QTC specifications:

- **Node** is the generic term for the store-and-forward daemon that sits between clients or radios and the rest of the network. The reference implementation is called **qtcd**. QTC is the Q-code for "I have messages for you."
- **Station** for a callsign means any node that has heard it and is holding state for it. It does not imply registration or permanence. A callsign may have several stations at once.
- **Public station** is a station with a reachable address. It typically also provides circuit relay for stations behind NAT, hosts mailboxes, and serves internet-only clients; `relay` is one of its capabilities, not a synonym for it.
- **QSL** is the informal name for a delivery receipt (RCPT).
- A client asking its station for waiting messages is, informally, **QRU?**

In this document:

- A **native device** sends and receives the QTC payload (§2): a radio whose firmware supports it, or a program such as `qtc chat`. A **legacy device** speaks only SMS (`0x05`).
- A device's **node** is the one it exchanges packets with directly: over RF through a gateway linked to the node, or over M17_inet as a client of the node's client face (Node Protocol §10). Everything in this document is single-hop between a device and its node.

## 1. Purpose

Legacy devices are terminals: a node transmits to them once and cannot tell whether anything arrived. Native devices can do better, and this document defines how:

- one M17 packet type for all of QTC, with the kind of QTC packet inside it (§2);
- acknowledgement and retry between a device and its node, so a message lost to a noisy RF path is sent again (§4), and, for room messages on RF, summaries that let radios ask for what they missed (§6.1);
- sync, so a device returning after an absence fetches what it missed, at its own pace (§5);
- how a node delivers to a native device, as opposed to a legacy one (§6).

Native devices use the transports that exist: M17 packet mode on RF, and M17_inet packet frames to a node's client face. No new transport, and no libp2p on the device. Hotspots continue to link to a node as they would to a reflector and pass packets through unchanged (Node Protocol §10); a gateway that routes QTC traffic to a node directly is future work and not described here.

## 2. The QTC Payload

QTC uses one M17 packet type, **QTC** (`0x08`, provisional). The byte after it says what kind of QTC packet it is:

| Offset | Size | Field | Description |
|-------:|-----:|-------|-------------|
| 0      | 1    | Type  | M17 packet type = QTC (`0x08`) |
| 1      | 1    | Kind  | See below |
| 2      | var  | Body  | The kind's layout, starting with its Version byte |

| Kind   | Name | Layout | Direction |
|-------:|------|--------|-----------|
| `0x00` | —    | reserved | — |
| `0x01` | MSG  | Message Envelope §4.1, from Version on | both |
| `0x02` | RCPT | Message Envelope §5.1, from Version on | both |
| `0x03` | ROOM | Rooms §5.1, from Version on | both |
| `0x04` | SYNC | §5.1 | both |
| `0x05` | ACK  | §4.1 | both |

A receiver ignores a kind it does not know, so kinds can be added without a new M17 packet type. MSG, RCPT, and ROOM keep their existing layouts, shifted one byte to make room for Kind. The message ID (Message Envelope §4.3) still hashes Version through Body, so it is unchanged: the same message has the same ID whether it is carried in this payload or in the node-to-node protocol. The maximum payload is still 823 bytes, so the MSG body limit drops by one byte, to 799 unsigned and 735 signed.

This replaces the separate MSG (`0x08`), RCPT (`0x09`), and ROOM (`0x0A`) packet types, so QTC asks the M17 working group for one packet type instead of three or more.

A packet's LSF carries its addressing:

- **MSG and RCPT:** LSF Source is the envelope's Source. From a device, LSF Destination is the envelope's Destination. From a node, it is the device the packet is for, suffix included, whatever the envelope's Destination, since a radio may show only what is addressed to its own callsign; a room MSG on RF, which serves every radio in range, goes to the room address (§6).
- **ROOM, SYNC, and ACK** are control packets. From a device, LSF Source is the device and LSF Destination is the node's callsign or the broadcast address (`0xFFFFFFFFFFFF`). The broadcast address lets a radio that has not yet learned its node's callsign reach whatever node hears it. From a node, LSF Source is the node's callsign and LSF Destination is the device. A node handles a control packet addressed to its own callsign or to broadcast, and never forwards one.

**Signatures.** A signature covers a context string and the Kind as well as the packet's own bytes:

```
SigningInput = "QTC" ‖ Kind ‖ Version ‖ … ‖ (end of the signed fields), SIGNED bit cleared
```

`"QTC"` is the three ASCII bytes `51 54 43`. Neither the string nor a second copy of Kind is transmitted; the signer and verifier supply them. Without Kind, one signed byte string could be valid as more than one kind: a signed MSG with a body of 9 bytes or more is also a correctly signed RCPT from the same sender, since the two layouts share their first fields. The context string separates QTC envelope signatures from anything else the same key signs: M17 voice (Message Envelope §4.6), and mailbox records, which a node's key signs under their own context string (Node Protocol §4). A signed MSG's digest is therefore no longer the hash its message ID is taken from; a signer computes both.

**Native or legacy.** The payload type says which a device is. A node treats a device as native from the first QTC payload it receives from it, and as legacy from the first SMS, and keeps that for as long as it remembers the device. A device the node has heard only on voice is legacy until it sends a QTC payload, which is one reason a native device syncs when it arrives (§5.3).

## 3. Addressing Devices

A native device is addressed by its full callsign, suffix included (Architecture §3). A native radio should use a distinct suffix per device, so its node can tell one device's acknowledgements from another's.

## 4. Acknowledgement and Retry

Every MSG between a device and its node is acknowledged. The rules are the same in both directions:

| Received | Acknowledged with |
|----------|-------------------|
| A MSG addressed to a callsign, delivered by a node to a device | RCPT DELIVERED from the device (§4.2) |
| A MSG from a device to its node | ACK listing its message ID (§4.1) |
| A room MSG from a node, over M17_inet | ACK listing its message ID |
| A room MSG from a node, on RF | Not acknowledged, except in a sync page, which goes to one device. Otherwise summaries and FETCH repair losses (§6.1). |
| RCPT | Not acknowledged. Receipts are best-effort (Message Envelope §5.3). |
| ROOM, or SYNC REQUEST | The reply. The requester repeats the request if no reply comes. |

A sender that has no acknowledgement after the ack timeout sends the packet again, up to the retry limit (§4.3). Retries are identical copies, so the receiver recognizes them by message ID and acknowledges again without acting twice. A node's ACK means it has accepted the message for delivery, not that the message has arrived anywhere. A node that refuses a device's MSG answers with RCPT REJECTED instead of an ACK, which also ends the retries.

When the retries run out:

- **Device to node:** the device reports that the message was not sent. The user can send it again later. It keeps its message ID, since it is the same envelope, so a copy that did get through is not duplicated.
- **Node to device:** the node treats the device as out of reach. It holds the message and anything that follows (Node Protocol §7.4) until it hears the device again (§5.3).

### 4.1 ACK

| Offset | Size | Field | Description |
|-------:|-----:|-------|-------------|
| 0      | 1    | Type  | QTC (`0x08`) |
| 1      | 1    | Kind  | ACK (`0x05`) |
| 2      | 1    | Version | `0x00` |
| 3      | 1    | Flags | Reserved, must be 0 |
| 4      | 1    | Count | Number of message IDs that follow, 1 or more |
| 5      | 8×n  | IDs   | Message IDs acknowledged |

One ACK may acknowledge several messages. On RF, a receiver may wait briefly to acknowledge a burst in one packet, but no longer than it takes a burst to finish, which is well inside the sender's ack timeout.

### 4.2 DELIVERED as acknowledgement

A native device answers every MSG addressed to a callsign with RCPT DELIVERED, whether or not the sender asked for a receipt. DELIVERED is the end-to-end status the envelope already defines, and it is also the hop acknowledgement, so the device sends one packet, not two. The node:

- records the message as delivered to that device (Node Protocol §7.4), from the DELIVERED rather than from the transmission, and stores a delivery record (Node Protocol §7.6);
- passes the DELIVERED on to the sender only if RCPT_REQ was set, so the rule of Message Envelope §5.3 still holds for senders.

Rooms generate no receipts (Rooms §7.5), so a room MSG is acknowledged with ACK where it is acknowledged at all.

### 4.3 Timing

| Parameter | RF | M17_inet |
|-----------|----|----------|
| Ack timeout, from the end of the transmission | 5 s | 2 s |
| Retries after the first send | 3 | 3 |
| Retry interval | ack timeout plus 0–2 s random | ack timeout |

These values are provisional. On RF the ack timeout has to cover the gateway's turnaround, including its receive holdoff and packet gap and anything queued ahead, and they will be revised from measurement. The random part keeps two senders that collided from colliding again.

## 5. Sync

A device returning after an absence asks its node for what it missed, a page at a time. A device pulls its backlog; the node does not push it. That leaves the device in control of how much RF airtime its backlog uses (Rooms Open Question 2).

### 5.1 SYNC

| Offset | Size | Field | Description |
|-------:|-----:|-------|-------------|
| 0      | 1    | Type  | QTC (`0x08`) |
| 1      | 1    | Kind  | SYNC (`0x04`) |
| 2      | 1    | Version | `0x00` |
| 3      | 1    | Flags | Requests only; see below. Other bits reserved, must be 0 |
| 4      | 1    | Op    | See below |
| 5      | 4    | Cursor | Position in the callsign's mailbox (§5.2) |
| 9      | 2    | Skip  | Entries at exactly Cursor to skip (§5.2) |
| 11     | 1    | Count | REQUEST: the most the device wants in one page. PAGE: how many follow. Otherwise 0. |
| 12     | 2    | Remaining | PAGE: how many are left after this page. NOTIFY: how many are waiting. `0xFFFF` means that many or more. Otherwise 0. |
| 14     | var  | Tail  | SUMMARY: room groups (§6.1). FETCH: 4-byte message IDs. REFUSED: UTF-8 reason. Otherwise empty. |

| Op     | Name     | Direction | Meaning |
|-------:|----------|-----------|---------|
| `0x00` | REQUEST  | device → node | Send the next page from Cursor and Skip |
| `0x01` | FETCH    | device → node | Send again the room messages whose IDs are in Tail (§6.1) |
| `0x80` | PAGE     | node → device | Reply: Count packets follow; Cursor and Skip are where the next request should start |
| `0x81` | NOTIFY   | node → device | Unsolicited: Remaining messages are waiting for this device |
| `0x82` | REFUSED  | node → device | Reply: the request was refused; Tail says why |
| `0x83` | SUMMARY  | node → device | Unsolicited, RF only: the room messages recently transmitted (§6.1) |

Fields an op does not use are 0.

Request flags:

| Bit | Name | Meaning |
|----:|------|---------|
| 0   | ALL  | Include messages this device has already acknowledged. A new device fetching history sets it. |
| 1   | SENT | Include messages the callsign sent, so its other devices see both sides of a conversation |

A page holds the MSGs, and the RCPTs to the callsign that are receipts for its own messages, from the device's base callsign's mailbox. Only those addressed to this device (to the base callsign, or to this exact device) are included, plus the callsign's sent messages if SENT is set. Unless ALL is set, messages this device has already acknowledged are left out. Delivery records are never included; they are for nodes (Node Protocol §7.6). Entries go oldest first. The device acknowledges each MSG as in §4 and, when the page is done, sends the next REQUEST with the Cursor and Skip from the PAGE. A PAGE with Remaining `0` ends the sync. The device keeps its Cursor and Skip for next time.

A node clamps Count to its own limit, by default 5 on RF and 50 over M17_inet. It sends the PAGE before the page's packets, so the device knows how many to expect. A device that never receives the PAGE repeats its REQUEST. A REQUEST that repeats one already answered gets the same page again, and the device drops the copies by message ID.

### 5.2 Cursor

Cursor is a received-at time in the mailbox (Node Protocol §5 QUERY), in Unix seconds on the node's clock. For a message that several mailbox members hold, the earliest time is used. Skip counts entries already sent that share exactly that second, so a page boundary inside one second neither repeats nor loses anything. Cursor `0` with Skip `0` means from the start of retention.

The device treats Cursor and Skip as opaque and only echoes them, so a radio without a clock can sync. Because they are mailbox times rather than one node's state, a device can continue a sync at a different node. Members' received-at times differ slightly and repairs make late copies, so the device may see a few repeats, which it drops by message ID.

### 5.3 Arrival

A native device sends a SYNC REQUEST with its saved Cursor and Skip when it starts, when it links to a node, and when it returns after being out of contact. The request marks the device as native (§2) and heard (Node Protocol §7.4). It also says the device is present and listening, which the reach window can only guess for a legacy radio (Architecture Open Question 1).

When a node hears a native device it has been holding messages for, without a SYNC REQUEST, for example on voice, it sends a single NOTIFY with the number waiting instead of replaying them. The device syncs when it chooses. The replay limit and its "older messages not sent" notice (Node Protocol §7.5) apply only to legacy devices.

## 6. Delivery to Native Devices

A node delivers to a native device as it does to a legacy one (Node Protocol §7.4), with these differences:

- **Native packets.** MSGs go out as the QTC payload, never translated to SMS, and signatures are kept. A room MSG keeps the room as its destination, with no `#NAME` text prefix (Rooms §6).
- **Acknowledged delivery.** A MSG counts as delivered to the device when its DELIVERED or ACK arrives (§4), not when it is transmitted. Without one after the retries, the message is held (§4). Room messages on RF are the exception (below).
- **Receipts.** RCPTs addressed to the device's callsign, such as QUEUED, TRANSMITTED, DELIVERED, EXPIRED, or REJECTED for messages it sent, are sent to it once, unacknowledged. Legacy devices never get them (Message Envelope §6).
- **Backlog.** A device that returns syncs (§5), and a device the node hears some other way gets a NOTIFY (§5.3). There is no replay limit.
- **Rooms on RF.** A room MSG is transmitted once, to the room address, whatever the number of subscribed devices in range (Rooms §7.4), and is not acknowledged. It counts as delivered to each subscribed device in reach when it is transmitted, as for a legacy radio. Losses are repaired as §6.1 describes.

A node that hears a native device also sends it SMS, and nothing else, if that device most recently sent an SMS (§2): one radio may send either.

### 6.1 Room Summaries

Acknowledging a multicast transmission radio by radio costs airtime in proportion to the number of radios, since each acknowledgement is a separate transmission with its own turnaround, and the acknowledgements collide. On RF, room messages therefore use negative acknowledgement: the node says what it sent, and a radio speaks only when it has missed something.

**Summaries.** A node that has transmitted room messages on RF sends a SYNC SUMMARY listing the most recent of them: at most 16, and none transmitted more than 30 minutes ago. It sends one at whichever comes first:

- 8 room messages transmitted since the last summary;
- the end of a burst: 5 s without transmitting a room message.

If no room message is transmitted in the 60 s after a burst-end summary, the node sends the summary once more. Together these give the invariant: **every room message transmitted on RF is listed in at least two summaries,** the second within about a minute. A summary waits for a clear channel like any other packet. A node sends none when it has transmitted no room message since the last one, or when no subscribed native device is in reach. Retransmissions answering FETCH are not new entries and do not count toward the 8.

The time limit keeps a radio that has just arrived from asking for traffic from before it was there (Rooms Open Question 2). Anything older comes from sync (§5).

**Summary layout.** SYNC with Op SUMMARY; Cursor, Skip, and Remaining are 0; Count is the number of room groups in Tail. Each group is:

| Size | Field | Description |
|-----:|-------|-------------|
| 6    | Room  | Room address |
| 1    | n     | Number of IDs that follow |
| 4×n  | IDs   | The first 4 bytes of each message ID, oldest first |

Four bytes are enough to tell apart the few messages one summary lists; a device matches them against the full IDs of what it has received.

**FETCH.** A device that hears a summary compares the entries for rooms it is subscribed to with what it has received. If any are missing, it waits a random 0–3 s. It then sends a SYNC FETCH, addressed as a control packet (§2), with Count set to the number of 4-byte IDs in Tail, leaving out any it has heard retransmitted while it waited. The node retransmits each listed message once, to the room address, which serves every radio that missed it, and ignores IDs it no longer has. A device still missing messages after the ack timeout may send FETCH again, up to the retry limit (§4.3), and otherwise leaves them to its next sync.

Over M17_inet each client has its own link, so room messages are acknowledged with ACK like any other (§4) and there are no summaries.

### 6.2 Defaults

| Parameter | Default |
|-----------|---------|
| Summary size | 16 room messages, none older than 30 min |
| Summary spacing | every 8 room messages, or 5 s after the last one, whichever is first |
| Summary repeat | once, 60 s after a burst-end summary if no room traffic follows |
| FETCH delay | random, 0–3 s after the summary |
| Sync page size | 5 on RF, 50 over M17_inet (§5.1) |
| Ack timeout, retries | §4.3 |

These are starting points to be revised from measurement on hotspots and radios.

## 7. Transports

**RF, through a hotspot.** The hotspot's gateway links to the node as it would to a reflector (Node Protocol §10). Gateways already forward packets of any type in both directions, so QTC payloads pass through unchanged. The node takes QTC payloads in on any module and never forwards them upstream.

**M17_inet.** A program links to the node's client face with CONN, usually on a messaging-only module, and exchanges QTC payloads in M17_inet packet frames. It learns the node's callsign from the node's ACKN and PING. The link keepalives say it is present (Node Protocol §10), so it is in reach while linked, and it syncs on each link (§5.3).

## 8. Security

Nothing here authenticates a device. A forged DELIVERED or ACK can make a node believe a message was delivered when it was not. A forged SYNC can make a node send a callsign's messages to whoever asks, but messages are not private anyway (Architecture §4). The protections are the ones legacy devices have: a gateway hears only what is in RF range, and a node's client face can limit internet clients by callsign. User keys (Architecture §3) will let a node verify a device's DELIVERED, ACK, and SYNC. The Flags byte in every kind leaves room for a signature.

## 9. Open Questions

1. **Timer values and sizes** (§4.3, §6.2), to be measured on hotspots and OpenRTX radios.
2. **One message across packets.** A body longer than one packet (the reserved FRAGMENT flag, Message Envelope §4.2) is out of scope. With per-frame losses measured so far, a shorter packet is more likely to arrive, so sending a long message as several short ones may be worth defining.

## 10. Resolved

- **One M17 packet type.** QTC uses a single packet type with a Kind byte, rather than one type per kind, so it asks the M17 working group for one value.
- **Native or legacy** is decided by the payload type a device sends; no separate announcement is needed.
- **Room acknowledgements on RF.** Per-radio acknowledgement of a multicast transmission costs airtime in proportion to the number of radios and gets worse as the channel does. Room messages on RF use summaries and FETCH instead (§6.1), so the cost grows with loss rather than with the number of radios.
- **Kind in the signature.** Signatures cover `"QTC"` and Kind (§2), so a signature for one kind is never valid for another, and QTC signatures are distinct from anything else the key signs. Message IDs are unaffected.
- **Rollout:** no compatibility period. Nodes and clients move to the QTC packet type and the new signing inputs in one release; records and stored envelopes in the old formats are not read.
