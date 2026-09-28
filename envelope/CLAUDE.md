# envelope package

Implements the QTC packet type (`0x08`, then a Kind byte): MSG and RCPT from `docs/qtc-envelope.md`, ROOM from `docs/qtc-rooms.md` §5, SYNC and ACK from `docs/qtc-client.md`. `PacketType` is the M17 byte (SMS, QTC); `Kind` is what the rest of the code switches on. `Kind.Stored()` is true only for MSG, RCPT and ROOM; SYNC and ACK are single-hop and never stored.

- `Envelope` holds the raw bytes. Accessors read from them; there is no settable field struct and no `Marshal` from fields. Construction goes through `Build*` functions that produce bytes once and return a parsed `Envelope`.
- `ID()` is SHA-256 over Version ‖ Flags-with-SIGNED-cleared ‖ Source ‖ Destination ‖ Timestamp ‖ TTL ‖ Nonce ‖ Body, first 8 bytes. The Type and Kind bytes are excluded. Cache it.
- `String()` gives the readable one-line form used in logs. `MarshalJSON` emits base64 of the raw bytes; `UnmarshalJSON` parses and validates.
- Signatures (MSG and RCPT only): ECDSA secp256r1 over SHA-256 of `"QTC" ‖ Kind ‖` the ID input (for RCPT, Version through Note), raw `r ‖ s` (64 bytes). The prefix is never transmitted, and the signed digest is not the ID hash. Sign with stdlib `crypto/ecdsa`; verify with `ecdsa.Verify`. The fixtures' signed examples must *verify* against the test key; do not attempt to reproduce their signature bytes, and do not implement RFC 6979 by hand.
- Reserved flag bits are preserved and hashed, never rejected. Unknown versions are rejected.
- Timestamp `0` means unknown; expiry is then the caller's problem, not this package's.
- Every function here has a test that runs `docs/qtc-fixtures.json`, including the `invalid_envelopes` section.
- stdlib only. No libp2p, no station, no m17 imports beyond address encoding if that lives in `m17`.
