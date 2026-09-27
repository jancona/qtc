# QTC: Message Envelope

*Part of QTC: An M17 Messaging System*

**Status:** Draft 0.1 — for discussion
**Scope:** Client ↔ node message and receipt packet types

## Terminology

This document is part of **QTC: An M17 Messaging System**. Throughout the QTC specifications:

- **Node** is the generic term for the store-and-forward daemon that sits between clients or radios and the rest of the network. The reference implementation is called **qtcd**. QTC is the Q-code for "I have messages for you."
- **Station** for a callsign means any node that has heard it and is holding state for it. It does not imply registration or permanence. A callsign may have several stations at once.
- **Public station** is a station with a reachable address. It typically also provides circuit relay for stations behind NAT, hosts mailboxes, and serves internet-only clients; `relay` is one of its capabilities, not a synonym for it.
- **QSL** is the informal name for a delivery receipt (RCPT).
- A client asking its station for waiting messages is, informally, **QRU?**

## 1. Purpose

This document defines two M17 packet mode payload types for store-and-forward text messaging:

- **MSG** — a text message with the metadata needed to identify, deduplicate, expire, and (optionally) sign it.
- **RCPT** — a receipt reporting the status of a previously sent MSG.

These types are carried unchanged over RF (M17 packet mode) and over IP (M17_inet packet frames). The node-to-node protocol that moves them between nodes is out of scope here and carries MSG and RCPT payloads opaquely.

The existing SMS type (0x05) remains valid. Nodes translate between SMS and MSG so that legacy clients and radios interoperate; see §6.

## 2. Conventions

- See Terminology above for node, station, and public station.
- All multi-byte integers are big-endian.
- "Callsign" means a 48-bit M17 base-40 encoded address as defined in the M17 specification.
- "Room" means an address in the M17 Extended address range (`0xEE6B28000000`–`0xFFFFFFFFFFFE`, which the M17 specification sets aside for application use). How room names are encoded into that range is defined in the Rooms specification.
- "Payload" means the packet mode contents including the packet type byte and excluding the trailing CRC-16, which the framing layer adds and strips. All offsets in this document are from the type byte.
- The maximum payload is 823 bytes.

## 3. Packet Types

| Name | Value  | Description |
|------|--------|-------------|
| MSG  | `0x08` | Text message envelope (§4) |
| RCPT | `0x09` | Message receipt (§5) |

These values are provisional. The M17 specification assigns `0x00`–`0x06`, and the 3.0.0 draft assigns `0x07` (TLE); `0x08` and `0x09` are the next unassigned values and will be proposed to the M17 working group once the design is further along. Implementations should keep the values easy to change until then. The M17 packet type specifier is formally a UTF-8-style variable-length integer; values below 128 occupy one byte, so all QTC types are single bytes.

## 4. MSG — Message Envelope

### 4.1 Layout

| Offset | Size | Field       | Description |
|-------:|-----:|-------------|-------------|
| 0      | 1    | Type        | Packet type = MSG |
| 1      | 1    | Version     | Envelope version. This document defines version `0x00`. |
| 2      | 1    | Flags       | See §4.2 |
| 3      | 6    | Source      | Sender callsign |
| 9      | 6    | Destination | Recipient callsign or room |
| 15     | 4    | Timestamp   | Origin time, unsigned seconds since the Unix epoch |
| 19     | 2    | TTL         | Time to live in minutes (§4.4) |
| 21     | 2    | Nonce       | Random value chosen by the sender (§4.3) |
| 23     | var  | Body        | UTF-8 text, no terminator (§4.5) |
| end−64 | 64   | Signature   | Present only if the SIGNED flag is set (§4.6) |

Fixed header length is 23 bytes. Body length is implicit: everything after the header, minus the signature if present.

### 4.2 Flags

| Bit | Name       | Meaning |
|----:|------------|---------|
| 0   | SIGNED     | A 64-byte signature follows the body |
| 1   | RCPT_REQ   | Sender requests a DELIVERED receipt |
| 2   | —          | Reserved for a future READ receipt request; must be 0 |
| 3   | FRAGMENT   | Reserved for future multi-packet messages; must be 0 |
| 4–7 | —          | Reserved, must be 0 |

Receivers must ignore reserved bits they do not understand rather than reject the message, so that flags can be added without a version bump.

### 4.3 Message ID

The message ID is not transmitted. It is **derived** from the envelope contents:

```
MessageID = SHA-256(Version ‖ Flags' ‖ Source ‖ Destination ‖ Timestamp ‖ TTL ‖ Nonce ‖ Body)[0:8]
```

where `Flags'` is the Flags byte with the SIGNED bit cleared. The signature is excluded, so a message has the same ID whether or not it is signed, and a node may strip a signature when delivering to a client that does not understand it without changing the ID.

Rationale:

- Content-derived IDs make deduplication independent of sender behavior: two copies of the same message arriving by different paths are recognized as one regardless of who forwarded them.
- Not transmitting the ID saves 8 bytes of airtime per message. Every node and client must compute it anyway to handle receipts.
- The nonce allows a sender to send the same text to the same destination twice within one second and have them treated as distinct messages. Senders should choose it randomly.

The first 64 bits of SHA-256 are sufficient: the ID only needs to be unique among messages that could plausibly coexist in a node's store or a client's history, and a 32-bit collision would already require billions of messages.

### 4.4 TTL and Expiry

`Expiry = Timestamp + (TTL × 60)` seconds.

- TTL `0` means "live only": nodes must not store the message. If the destination is not currently reachable it is dropped and, if RCPT_REQ is set, an EXPIRED receipt is returned immediately.
- TTL `0xFFFF` means "no sender preference": nodes apply their configured default, which is 7 days (10080 minutes) unless changed. As with an unknown timestamp, the expiry of such a message is determined by the node, not by the envelope.
- Nodes may cap TTL to a local maximum. If a node reduces TTL below the sender's request, it should report EXPIRED when its own limit is reached.
- Any node or client that receives a message past its expiry must discard it silently.

The timestamp is the origin time as known by the *sender*. A timestamp of `0`, or one earlier than a configured floor, means "unknown"; senders without a real-time clock (many radios) must use `0`. Nodes compute expiry for such messages from the time they first received it. Nodes receiving a message with a timestamp more than a configured tolerance in the future should likewise treat it as "now" for expiry purposes.

In no case may a node alter the envelope, since that would change the message ID. Anything a node needs to record about a message (receipt time, hop count, vouching) belongs in the node-to-node wrapper, not the envelope. The envelope is immutable end to end.

### 4.5 Body

UTF-8 encoded text. No terminator; length is implicit. Maximum length is 800 bytes unsigned, 736 bytes signed. Clients should count bytes, not characters, when enforcing limits.

The body should not contain leading or trailing whitespace; nodes may trim it when translating from SMS (§6) but must not modify a MSG body.

### 4.6 Signature

When SIGNED is set, the last 64 bytes of the payload are a signature over exactly the bytes hashed in §4.3 (the same input as the message ID). The algorithm is ECDSA on secp256r1, matching M17 stream signing (M17 specification §3.2.5), so that one key pair serves both voice and messaging. The message digest is SHA-256 of the canonical bytes, i.e. the same hash from which the message ID is taken. The signature is encoded as the raw concatenation `r ‖ s`, each a 32-byte big-endian integer; DER encoding is not used. Signers without a trustworthy entropy source (radios in particular) should use deterministic nonce generation (RFC 6979), since ECDSA leaks the private key on nonce reuse; a hedged construction that mixes RFC 6979 with randomness is equally acceptable. Verifiers must accept any valid signature and must not expect to reproduce one; interoperability is tested by verification, not by byte-equality of signatures.

Key discovery and trust are out of scope for this document. A node or client that cannot verify a signature must treat the message as unsigned rather than rejecting it, unless local policy requires signatures from that source.

### 4.7 Room Messages

A MSG whose Destination is a room is delivered to every client subscribed to that room. RCPT_REQ is ignored for room messages; no receipts are generated except REJECTED (§5.3).

## 5. RCPT — Receipt

### 5.1 Layout

| Offset | Size | Field       | Description |
|-------:|-----:|-------------|-------------|
| 0      | 1    | Type        | Packet type = RCPT |
| 1      | 1    | Version     | `0x00` |
| 2      | 1    | Flags       | Bit 0 = SIGNED; other bits reserved |
| 3      | 6    | Source      | Callsign issuing the receipt (a client or a node) |
| 9      | 6    | Destination | Source callsign of the original message |
| 15     | 8    | Message ID  | ID of the original message (§4.3) |
| 23     | 1    | Status      | See §5.2 |
| 24     | 4    | Timestamp   | Time the status was reached |
| 28     | 4    | Last heard  | When the issuing node last heard the recipient callsign; `0` = never or not applicable |
| 32     | var  | Note        | Optional UTF-8 text, typically a rejection reason |
| end−64 | 64   | Signature   | Present only if SIGNED |

### 5.2 Status Codes

| Value  | Name        | Issued by | Meaning |
|-------:|-------------|-----------|---------|
| `0x00` | QUEUED      | Node      | The sender's node reports that the message has been accepted for store-and-forward (at least one mailbox member has stored it). Sent at most once, by the sender's node; mailbox nodes never issue receipts. |
| `0x01` | TRANSMITTED | Node      | The message was transmitted on RF, or sent to a legacy client, toward the recipient. Receipt is not confirmed. Last heard indicates how recently the recipient was active at this node. |
| `0x02` | DELIVERED   | Client    | The recipient's client received the message. Never issued by a node. |
| `0x03` | —           | —         | Reserved for a future READ status. |
| `0x04` | EXPIRED     | Node      | The message reached its expiry without delivery. Last heard indicates when the recipient was last active, if ever. |
| `0x05` | REJECTED    | Node or client | The message was refused. The Note may say why. |

### 5.3 Rules

- Receipts never generate receipts.
- A receipt may be signed (§5.4); DELIVERED by the recipient's key once user keys exist, TRANSMITTED/QUEUED/EXPIRED by the node's key.
- QUEUED, TRANSMITTED, EXPIRED, and DELIVERED are sent only if RCPT_REQ was set. REJECTED may always be sent. This governs receipts to the sender. Separately, a node may store delivery records in the recipient's own mailbox (Node Protocol §7.6): RCPTs addressed to the recipient, marked by their note, used only between nodes and never delivered as receipts.
- A receipt's Source is the callsign of whoever observed the status: the node's callsign for QUEUED, TRANSMITTED, and EXPIRED; the recipient's callsign for DELIVERED.
- A message may produce both TRANSMITTED (from the node) and DELIVERED (from a client that speaks MSG). A legacy radio produces only TRANSMITTED.
- Receipts are best-effort. Nodes may store them for delivery back to the sender, but not beyond the original message's expiry.
- A receiver that gets a receipt for a message ID it does not recognize discards it.

### 5.4 Signature

When SIGNED is set, the last 64 bytes of the payload are a signature, in the form of §4.6, over the bytes from Version through the end of the Note with the SIGNED bit cleared in Flags (i.e. the same shape as the MSG signing input, with the Type byte excluded). Receipts have no message ID of their own and are never deduplicated by content.

## 6. Interoperation with SMS (0x05)

Legacy clients and radios send and receive SMS. Nodes translate at the boundary.

**SMS → MSG (ingress).** A node receiving an SMS from a client or the RF side constructs a MSG with:

- Version `0x00`, Flags `0x00`
- Source and Destination from the LSF of the received packet
- Timestamp = current time at the node
- TTL = node default
- Nonce = random
- Body = the SMS text up to the first NUL, with leading and trailing ASCII whitespace (space, tab, CR, LF) trimmed

Because the node minted the timestamp and nonce, a legacy sender cannot request or receive receipts. A node may present message status to legacy users by other means (a local web page, a synthesized SMS reply) but this is a node feature, not part of the protocol.

**MSG → SMS (egress).** A node delivering a MSG to a legacy client or radio emits an SMS whose LSF Source and Destination are the envelope's Source and Destination and whose text is the Body followed by a NUL. Signatures are dropped. If RCPT_REQ was set, the node issues TRANSMITTED once the SMS has been sent.

**Receipts to legacy clients.** Dropped. Optionally rendered as SMS text at the node's discretion.

## 7. Example

A MSG from N1ADJ to W1AW, 24-hour TTL, delivery receipt requested, unsigned:

```
Offset  Bytes                          Field
0       08                             Type = MSG
1       00                             Version 0
2       02                             Flags: RCPT_REQ
3       00 00 01 8A 92 AE              Source: N1ADJ
9       00 00 00 16 80 B7              Destination: W1AW
15      6A A3 ED 40                    Timestamp: 2026-09-11 12:00:00 UTC
19      05 A0                          TTL: 1440 minutes
21      3C 7F                          Nonce
23      48 69 20 4A 69 6D ...          Body: "Hi Jim, testing the new envelope."
```

The message ID is `SHA-256(00 02 0000018A92AE 0000001680B7 6AA3ED40 05A0 3C7F "Hi Jim, …")[0:8]` = `CBA5C5C74EAEBF72`. This example, with many others, is in `qtc-fixtures.json`; all values there are generated by an independent reference implementation (`qtc-fixtures-gen.py`).

If W1AW's home node sent the message to a legacy radio, it would issue a TRANSMITTED receipt with Source = the node's callsign, Destination = N1ADJ, that Message ID, Status `0x01`, the node's timestamp, and W1AW's last-heard time. If instead W1AW's client speaks MSG, it would issue DELIVERED (`0x02`) with Source = W1AW.

## 8. Open Questions

None at present.

## 9. Resolved

- **Packet type values:** provisionally `0x08` (MSG) and `0x09` (RCPT), the next unassigned values after `0x00`–`0x07` (verified against the M17 specification `main` and 3.0.0 `dev` sources, September 2026). Formal assignment deferred until the design is further along.
- **Derived vs. transmitted message ID:** derived. The envelope is immutable end to end (§4.4); anything a node needs to add goes in the node-to-node wrapper.
- **Node-issued DELIVERED:** replaced by a distinct TRANSMITTED status carrying the recipient's last-heard time. DELIVERED is client-only.
- **READ receipts:** omitted from v0. Flag bit 2 and status `0x03` are reserved so they can be added later without a version bump.
- **Default TTL:** 7 days. Nodes may configure a different default or cap.
- **Fragmentation:** not supported in v0; a single packet (800 bytes unsigned, 736 signed) is the message size limit. The FRAGMENT flag stays reserved in case that ever changes.
- **Room encoding:** out of scope. This document only requires that a room destination lie in the M17 reserved address range; the encoding, naming, and subscription semantics belong in a separate rooms specification.
- **Signature algorithm:** ECDSA secp256r1, per M17 specification §3.2.5, raw `r ‖ s` encoding (§4.6).
