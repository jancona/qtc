#!/usr/bin/env python3
"""Generate QTC test fixtures.

This is an independent reference implementation of the QTC Message
Envelope and Rooms encodings, used to produce fixtures that the Go
implementation (and any other, e.g. OpenRTX) must reproduce exactly.

Everything here follows:
  - M17 Specification (spec.m17project.org), address encoding appendix
  - QTC: Message Envelope, draft 0.1
  - QTC: Rooms, draft 0.1
"""
import hashlib, json, os, struct

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature

# ---------------------------------------------------------------- M17 addresses

ALPHABET = " ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-/."
EXTENDED_BASE = 0xEE6B28000000
EXTENDED_END = 0xFFFFFFFFFFFE
BROADCAST = 0xFFFFFFFFFFFF


def b40_encode(text: str) -> int:
    """M17 base-40: first character is least significant."""
    assert len(text) <= 9, text
    v = 0
    for ch in reversed(text):
        v = v * 40 + ALPHABET.index(ch)
    return v


def b40_decode(v: int) -> str:
    out = []
    while v:
        out.append(ALPHABET[v % 40])
        v //= 40
    return "".join(out)


def addr_bytes(v: int) -> bytes:
    return v.to_bytes(6, "big")


def base_callsign(text: str) -> str:
    """Architecture §3: everything before the first space or '-'. '/' is not a separator."""
    for i, ch in enumerate(text):
        if ch in " -":
            return text[:i]
    return text


# ---------------------------------------------------------------- Rooms

ROOM_ALPHABET = set("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-")


def room_valid_name(name: str) -> bool:
    return 1 <= len(name) <= 8 and all(c in ROOM_ALPHABET for c in name.upper())


def room_encode(name: str) -> int:
    name = name.upper()
    assert room_valid_name(name), name
    return EXTENDED_BASE + b40_encode(name)


def room_decode(v: int):
    """Returns the room name, or None if the address is not a valid room."""
    if v <= EXTENDED_BASE or v > EXTENDED_END:
        return None
    name = b40_decode(v - EXTENDED_BASE)
    if len(name) > 8 or not all(c in ROOM_ALPHABET for c in name):
        return None
    return name


# ---------------------------------------------------------------- Envelope

PT_MSG, PT_RCPT, PT_ROOM = 0x08, 0x09, 0x0A
F_SIGNED, F_RCPT_REQ = 0x01, 0x02
ST_QUEUED, ST_TRANSMITTED, ST_DELIVERED, ST_EXPIRED, ST_REJECTED = 0, 1, 2, 4, 5
OP_JOIN, OP_LEAVE, OP_LIST, OP_OK, OP_REFUSED = 0, 1, 2, 0x80, 0x81


def msg_signing_input(version, flags, src, dst, ts, ttl, nonce, body) -> bytes:
    """Envelope §4.3: Version ‖ Flags' ‖ Source ‖ Destination ‖ Timestamp ‖ TTL ‖ Nonce ‖ Body,
    where Flags' has the SIGNED bit cleared. The Type byte is NOT included."""
    return (bytes([version, flags & ~F_SIGNED]) + addr_bytes(src) + addr_bytes(dst)
            + struct.pack(">IHH", ts, ttl, nonce) + body)


def msg_id(version, flags, src, dst, ts, ttl, nonce, body) -> bytes:
    return hashlib.sha256(msg_signing_input(version, flags, src, dst, ts, ttl, nonce, body)).digest()[:8]


def build_msg(src, dst, ts, ttl, nonce, body: bytes, flags=0, sig: bytes = b"", version=0) -> bytes:
    if sig:
        flags |= F_SIGNED
    assert (flags & F_SIGNED) == bool(sig)
    hdr = bytes([PT_MSG, version, flags]) + addr_bytes(src) + addr_bytes(dst) + struct.pack(">IHH", ts, ttl, nonce)
    return hdr + body + sig


def build_rcpt(src, dst, mid: bytes, status, ts, last_heard=0, note: bytes = b"", flags=0, version=0) -> bytes:
    return (bytes([PT_RCPT, version, flags]) + addr_bytes(src) + addr_bytes(dst) + mid
            + bytes([status]) + struct.pack(">II", ts, last_heard) + note)


def build_room(op, ts, rooms, note: bytes = b"", version=0, flags=0) -> bytes:
    return bytes([PT_ROOM, version, flags, op]) + struct.pack(">I", ts) + bytes([len(rooms)]) + b"".join(addr_bytes(r) for r in rooms) + note


def sign(priv, data: bytes) -> bytes:
    """ECDSA secp256r1 over SHA-256(data), raw r‖s. RFC 6979 deterministic nonces."""
    der = priv.sign(data, ec.ECDSA(hashes.SHA256(), deterministic_signing=True))
    r, s = decode_dss_signature(der)
    return r.to_bytes(32, "big") + s.to_bytes(32, "big")


# ---------------------------------------------------------------- Fixtures

def hx(b: bytes) -> str:
    return b.hex()


def main():
    fx = {
        "qtc_fixtures_version": 3,
        "notes": [
            "All byte strings are lowercase hex. All integers are decimal.",
            "Packet types are provisional (Envelope spec §3).",
            "Message IDs hash Version..Body with the SIGNED flag cleared; the Type byte is excluded (Envelope §4.3).",
            "Signatures are ECDSA secp256r1 over SHA-256 of the same bytes, raw r||s, RFC 6979 deterministic nonces (Envelope §4.6). RCPT signing input is Version..Note with SIGNED cleared (§5.4).",
            "expiry is null when it is not determined by the envelope alone (timestamp 0 or TTL 0xFFFF); tests must not check it then.",
            "base_callsign is the mechanical normalization only; callers apply the rule that node callsigns are used whole.",
        ],
        "packet_types": {"MSG": PT_MSG, "RCPT": PT_RCPT, "ROOM": PT_ROOM},
        "flags": {"SIGNED": F_SIGNED, "RCPT_REQ": F_RCPT_REQ},
        "rcpt_status": {"QUEUED": 0, "TRANSMITTED": 1, "DELIVERED": 2, "EXPIRED": 4, "REJECTED": 5},
        "room_ops": {"JOIN": 0, "LEAVE": 1, "LIST": 2, "OK": 0x80, "REFUSED": 0x81},
    }

    # --- addresses
    addrs = ["N1ADJ", "N1ADJ  H", "N1ADJ  P", "N1ADJ-7", "W1AW", "K1XYZ  R", "VE3/N1ADJ", "AB1CD", "A", "........."]
    fx["addresses"] = []
    for a in addrs:
        v = b40_encode(a)
        fx["addresses"].append({
            "text": a, "value": v, "hex": hx(addr_bytes(v)),
            "base_callsign": base_callsign(a),
            "device_suffix": a[len(base_callsign(a)):] if a != base_callsign(a) else "",
        })
    fx["special_addresses"] = {
        "reserved_zero": hx(addr_bytes(0)),
        "standard_max": hx(addr_bytes(0xEE6B27FFFFFF)),
        "extended_base": hx(addr_bytes(EXTENDED_BASE)),
        "extended_end": hx(addr_bytes(EXTENDED_END)),
        "broadcast": hx(addr_bytes(BROADCAST)),
        "trailing_space_note": "ABC, 'ABC ' and 'ABC      ' all encode to " + hx(addr_bytes(b40_encode("ABC"))),
    }

    # --- rooms
    fx["rooms"] = {"valid": [], "invalid_names": ["", "MAINELOBSTER", "MAINE/1", "MA.INE", "MA INE"], "invalid_addresses": []}
    for name in ["MAINE", "M17DEV", "K1XYZ", "N1ADJ", "ABCDEFGH", "A", "R-1"]:
        v = room_encode(name)
        fx["rooms"]["valid"].append({"name": name, "canonical": name.upper(), "value": v, "hex": hx(addr_bytes(v)),
                                     "decodes_to": room_decode(v)})
    fx["rooms"]["valid"].append({"name": "maine", "canonical": "MAINE", "value": room_encode("maine"),
                                 "hex": hx(addr_bytes(room_encode("maine"))), "decodes_to": "MAINE",
                                 "note": "case-insensitive; canonicalized to upper"})
    for v, why in [(EXTENDED_BASE, "empty name (offset zero) is invalid"),
                   (EXTENDED_BASE + b40_encode("AAAAAAAAA"), "decodes to 9 characters"),
                   (EXTENDED_BASE + b40_encode("MAINE/1"), "contains /"),
                   (0xF46109000000, "first address of the range reserved by Rooms §3.3"),
                   (EXTENDED_END, "last Extended address; reserved"),
                   (b40_encode("MAINE"), "a Standard-range address is not a room"),
                   (BROADCAST, "broadcast")]:
        fx["rooms"]["invalid_addresses"].append({"hex": hx(addr_bytes(v)), "reason": why, "decodes_to": room_decode(v)})
    fx["rooms"]["range"] = {"first_valid": hx(addr_bytes(EXTENDED_BASE + 1)),
                            "last_valid": hx(addr_bytes(EXTENDED_BASE + 40**8 - 1)),
                            "first_reserved_by_rooms_spec": hx(addr_bytes(0xF46109000000))}
    assert EXTENDED_BASE + 40**8 - 1 == 0xF46108FFFFFF

    # --- envelopes
    N1ADJ, N1ADJ_H, N1ADJ_P, W1AW, K1XYZ_R = (b40_encode(x) for x in ["N1ADJ", "N1ADJ  H", "N1ADJ  P", "W1AW", "K1XYZ  R"])
    MAINE = room_encode("MAINE")
    T0 = 1789128000  # 2026-09-11 12:00:00 UTC
    envs = []

    def add_msg(name, desc, src, dst, ts, ttl, nonce, body, flags=0, sig=b"", **extra):
        raw = build_msg(src, dst, ts, ttl, nonce, body, flags, sig)
        f = flags | (F_SIGNED if sig else 0)
        e = {"name": name, "description": desc, "type": "MSG", "bytes": hx(raw), "length": len(raw),
             "fields": {"version": 0, "flags": f, "source": hx(addr_bytes(src)), "destination": hx(addr_bytes(dst)),
                        "timestamp": ts, "ttl_minutes": ttl, "nonce": nonce, "body": body.decode("utf-8")},
             "signing_input": hx(msg_signing_input(0, f, src, dst, ts, ttl, nonce, body)),
             "message_id": hx(msg_id(0, f, src, dst, ts, ttl, nonce, body)),
             "expiry": None if (ts == 0 or ttl == 0xFFFF) else ts + ttl * 60}
        e.update(extra)
        envs.append(e)
        return e

    ex = add_msg("msg_basic", "Envelope spec §7 example: N1ADJ→W1AW, 24h TTL, delivery receipt requested, unsigned",
                 N1ADJ, W1AW, T0, 1440, 0x3C7F, b"Hi Jim, testing the new envelope.", flags=F_RCPT_REQ)
    add_msg("msg_from_device", "Source carries a device suffix; base callsign is N1ADJ",
            N1ADJ_H, W1AW, T0 + 60, 1440, 0x0001, b"Sent from the HT")
    add_msg("msg_to_device", "Destination carries a device suffix: deliver only to N1ADJ's P device",
            W1AW, N1ADJ_P, T0 + 120, 1440, 0x0002, b"Only for the phone")
    add_msg("msg_unknown_timestamp", "Clock-less radio: timestamp 0; expiry computed from receipt time (§4.4)",
            N1ADJ_H, W1AW, 0, 1440, 0xBEEF, b"No RTC here")
    add_msg("msg_live_only", "TTL 0: must not be stored",
            N1ADJ, W1AW, T0 + 180, 0, 0x0003, b"Are you there?")
    add_msg("msg_default_ttl", "TTL 0xFFFF: node applies its default (7 days)",
            N1ADJ, W1AW, T0 + 240, 0xFFFF, 0x0004, b"Whenever you get this")
    add_msg("msg_to_room", "Room destination; no receipts generated (§4.7)",
            N1ADJ, MAINE, T0 + 300, 1440, 0x0005, "Net tonight at 7 — 146.52 \u2192 M17".encode("utf-8"), flags=F_RCPT_REQ,
            note="RCPT_REQ set but ignored for rooms")
    add_msg("msg_empty_body", "Zero-length body is legal",
            N1ADJ, W1AW, T0 + 360, 60, 0x0006, b"")
    add_msg("msg_max_body_unsigned", "800-byte body: the maximum unsigned (§4.5)",
            N1ADJ, W1AW, T0 + 420, 1440, 0x0007, bytes(((i % 26) + 65) for i in range(800)))
    add_msg("msg_same_text_twice", "Same fields as msg_basic except nonce: different message ID",
            N1ADJ, W1AW, T0, 1440, 0x3C80, b"Hi Jim, testing the new envelope.", flags=F_RCPT_REQ)
    add_msg("msg_reserved_flags", "Reserved flag bits set: receivers must ignore them, and they ARE part of the hash",
            N1ADJ, W1AW, T0 + 480, 1440, 0x0008, b"Future flags", flags=0xF0)

    # --- signed
    priv = ec.derive_private_key(0x1F2E3D4C5B6A79880F1E2D3C4B5A69784F5E6D7C8B9AA9B8C7D6E5F40312AB01, ec.SECP256R1())
    pub = priv.public_key()
    nums = pub.public_numbers()
    body = b"Signed hello"
    flags = F_RCPT_REQ
    si = msg_signing_input(0, flags, N1ADJ, W1AW, T0 + 540, 1440, 0x5151, body)
    sig = sign(priv, si)
    pub.verify(bytes(0) or __import__("cryptography.hazmat.primitives.asymmetric.utils", fromlist=["encode_dss_signature"]).encode_dss_signature(int.from_bytes(sig[:32], "big"), int.from_bytes(sig[32:], "big")), si, ec.ECDSA(hashes.SHA256()))
    e = add_msg("msg_signed", "SIGNED|RCPT_REQ. Signature over signing_input; message ID identical whether or not signed",
                N1ADJ, W1AW, T0 + 540, 1440, 0x5151, body, flags=flags, sig=sig,
                signature=hx(sig), digest=hx(hashlib.sha256(si).digest()))
    add_msg("msg_signed_stripped", "msg_signed with the signature removed by a node: same message ID",
            N1ADJ, W1AW, T0 + 540, 1440, 0x5151, body, flags=flags)
    assert envs[-1]["message_id"] == e["message_id"]
    fx["test_key"] = {
        "curve": "secp256r1",
        "private_d": hx(priv.private_numbers().private_value.to_bytes(32, "big")),
        "public_x": hx(nums.x.to_bytes(32, "big")), "public_y": hx(nums.y.to_bytes(32, "big")),
        "public_uncompressed": hx(pub.public_bytes(serialization.Encoding.X962, serialization.PublicFormat.UncompressedPoint)),
        "public_spki_der": hx(pub.public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)),
        "note": "TEST KEY ONLY. Fixture signatures were produced with RFC 6979 deterministic nonces. Implementations must VERIFY them against this key; they are not expected to reproduce the bytes, and signers may use randomized or hedged nonces.",
    }

    # --- receipts
    mid = bytes.fromhex(ex["message_id"])
    rcpts = []
    def add_rcpt(name, desc, src, dst, status, ts, last_heard=0, note=b""):
        raw = build_rcpt(src, dst, mid, status, ts, last_heard, note)
        rcpts.append({"name": name, "description": desc, "type": "RCPT", "bytes": hx(raw), "length": len(raw),
                      "fields": {"version": 0, "flags": 0, "source": hx(addr_bytes(src)), "destination": hx(addr_bytes(dst)),
                                 "message_id": hx(mid), "status": status, "timestamp": ts, "last_heard": last_heard,
                                 "note": note.decode("utf-8")}})
    add_rcpt("rcpt_queued", "Node K1XYZ R accepted msg_basic for store-and-forward", K1XYZ_R, N1ADJ, ST_QUEUED, T0 + 1)
    add_rcpt("rcpt_transmitted", "Node transmitted toward W1AW; W1AW last heard 20 minutes earlier", K1XYZ_R, N1ADJ, ST_TRANSMITTED, T0 + 30, T0 - 1200)
    add_rcpt("rcpt_delivered", "W1AW's client received msg_basic (client-issued, no last_heard)", W1AW, N1ADJ, ST_DELIVERED, T0 + 45)
    add_rcpt("rcpt_expired", "Expired; W1AW never heard", K1XYZ_R, N1ADJ, ST_EXPIRED, T0 + 86400, 0)
    add_rcpt("rcpt_rejected", "Rejected with a note", K1XYZ_R, N1ADJ, ST_REJECTED, T0 + 2, 0, b"callsign blocked")
    # signed receipt: signature over Version..Note with SIGNED cleared (Envelope §5.4)
    unsigned = build_rcpt(W1AW, N1ADJ, mid, ST_DELIVERED, T0 + 46)
    rsi = unsigned[1:]  # Version through Note; flags byte already 0
    rsig = sign(priv, rsi)
    signed = bytes([PT_RCPT, 0, F_SIGNED]) + unsigned[3:] + rsig
    rcpts.append({"name": "rcpt_delivered_signed", "description": "DELIVERED with SIGNED; signing input is Version..Note with SIGNED cleared",
                  "type": "RCPT", "bytes": hx(signed), "length": len(signed),
                  "fields": {"version": 0, "flags": F_SIGNED, "source": hx(addr_bytes(W1AW)), "destination": hx(addr_bytes(N1ADJ)),
                             "message_id": hx(mid), "status": ST_DELIVERED, "timestamp": T0 + 46, "last_heard": 0, "note": ""},
                  "signing_input": hx(rsi), "signature": hx(rsig), "digest": hx(hashlib.sha256(rsi).digest())})
    fx["envelopes"] = envs + rcpts

    # --- room packets
    rp = []
    def add_room(name, desc, op, ts, rooms, note=b""):
        raw = build_room(op, ts, rooms, note)
        rp.append({"name": name, "description": desc, "type": "ROOM", "bytes": hx(raw), "length": len(raw),
                   "fields": {"version": 0, "flags": 0, "op": op, "timestamp": ts, "count": len(rooms),
                              "rooms": [hx(addr_bytes(r)) for r in rooms], "note": note.decode("utf-8")}})
    add_room("room_join", "JOIN MAINE and M17DEV", OP_JOIN, T0 + 600, [MAINE, room_encode("M17DEV")])
    add_room("room_join_no_clock", "JOIN with timestamp 0; node substitutes receipt time before storing", OP_JOIN, 0, [MAINE])
    add_room("room_leave", "LEAVE MAINE", OP_LEAVE, T0 + 660, [MAINE])
    add_room("room_list", "LIST request: no rooms", OP_LIST, T0 + 720, [])
    add_room("room_ok_empty", "OK reply to JOIN/LEAVE", OP_OK, T0 + 601, [])
    add_room("room_ok_list", "OK reply to LIST: current subscriptions", OP_OK, T0 + 721, [MAINE, room_encode("K1XYZ")])
    add_room("room_refused", "REFUSED with note; the refused room is listed", OP_REFUSED, T0 + 602, [room_encode("BADROOM")], b"room not carried")
    fx["room_packets"] = rp

    # --- SMS wrapping (Envelope §6)
    sms = bytes([0x05]) + b"  Hello from a legacy radio  \x00"
    ts, nonce = T0 + 900, 0x7A7A
    body = sms[1:].split(b"\x00", 1)[0].strip(b" \t\r\n")
    raw = build_msg(N1ADJ_H, W1AW, ts, 10080, nonce, body)
    fx["sms_wrap"] = {
        "description": "Legacy SMS from LSF src 'N1ADJ  H' to dst 'W1AW' received at node time T0+900; node default TTL 7 days; nonce chosen as shown",
        "sms_packet": hx(sms), "lsf_source": hx(addr_bytes(N1ADJ_H)), "lsf_destination": hx(addr_bytes(W1AW)),
        "node_time": ts, "node_default_ttl_minutes": 10080, "nonce": nonce,
        "envelope": hx(raw), "message_id": hx(msg_id(0, 0, N1ADJ_H, W1AW, ts, 10080, nonce, body)),
        "unwrap": {"description": "Egress of msg_basic to a legacy client: SMS type byte, body, NUL",
                   "sms_packet": hx(bytes([0x05]) + b"Hi Jim, testing the new envelope." + b"\x00")},
    }

    # --- invalid envelopes
    fx["invalid_envelopes"] = [
        {"name": "truncated_header", "bytes": hx(bytes.fromhex(ex["bytes"])[:20]), "reason": "shorter than the 23-byte MSG header"},
        {"name": "signed_flag_too_short", "bytes": hx(bytes([PT_MSG, 0, F_SIGNED]) + addr_bytes(N1ADJ) + addr_bytes(W1AW) + struct.pack(">IHH", T0, 60, 1) + b"short"),
         "reason": "SIGNED set but payload shorter than header + 64"},
        {"name": "unknown_version", "bytes": hx(bytes([PT_MSG, 1, 0]) + addr_bytes(N1ADJ) + addr_bytes(W1AW) + struct.pack(">IHH", T0, 60, 1) + b"v1"),
         "reason": "version 1 is not defined; must be rejected (or handled by a future parser)"},
        {"name": "body_too_long", "bytes": hx(build_msg(N1ADJ, W1AW, T0, 60, 1, b"A" * 801)), "reason": "801-byte body exceeds the 823-byte packet"},
        {"name": "rcpt_truncated", "bytes": hx(build_rcpt(K1XYZ_R, N1ADJ, mid, ST_QUEUED, T0)[:30]), "reason": "shorter than the 32-byte RCPT header"},
        {"name": "room_count_mismatch", "bytes": hx(bytes([PT_ROOM, 0, 0, OP_JOIN]) + struct.pack(">I", T0) + bytes([2]) + addr_bytes(MAINE)),
         "reason": "count says 2 rooms, only one present"},
    ]

    os.makedirs("/mnt/user-data/outputs", exist_ok=True)
    with open("/mnt/user-data/outputs/qtc-fixtures.json", "w") as f:
        json.dump(fx, f, indent=2, ensure_ascii=False)
    print("ok", len(fx["envelopes"]), "envelopes")
    # sanity against hand-computed values in the envelope spec example
    assert hx(addr_bytes(N1ADJ)) == "0000018a92ae", hx(addr_bytes(N1ADJ))
    assert hx(addr_bytes(W1AW)) == "0000001680b7"
    assert T0 == 0x6AA3ED40
    print("example bytes:", ex["bytes"][:60], "id:", ex["message_id"])


if __name__ == "__main__":
    main()
