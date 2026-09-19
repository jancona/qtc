# roost package

The daemon. Implements `docs/pigeon-node-protocol.md` on go-libp2p plus the M17_inet proxy side described in `docs/pigeon-architecture.md` §2.

- Two faces: M17_inet (UDP, reflector-compatible) toward gateways and clients; libp2p toward other nodes. One flat package: keep the faces in separate files (`inet.go`, `p2p.go`) with a narrow interface between them, but no sub-packages, which invite import cycles over shared types.
- Room subscription state (rooms spec §4: latest ROOM envelope per room wins, sticky opt-outs, expiry) lives here in `rooms.go`, not in its own package.
- Client face (`inet.go`, node protocol §10): the roost is one M17_inet reflector (e.g. `M17-PIG`) on one UDP port. Each module letter maps to an upstream reflector+module and a mode: `native` is a pure proxy, `pigeon` takes SMS/MSG/RCPT/ROOM into Pigeon and never forwards them, and drops messaging arriving from upstream. Unmapped modules get NACK. Voice always passes through untouched. Framing, LSF, and CRC are implemented locally in `m17frame.go` rather than importing `m17`, whose root package links modem, audio, serial, and ZeroMQ dependencies; revisit if `m17` grows a lean core package.
- Presence is published truthfully with `via` (RF, local client, internet client). Never fabricate presence.
- Homing a callsign: publish presence → read or create inbox record → WATCH members → sweep → replay within the replay window for messages without DELIVERED. See node protocol §7.
- Only the loft writes inbox records. Repair requires a member to be both unreachable across three sweeps and silent in presence for the failure period. Log every record write with old and new versions.
- Never deliver the same message ID to the same device twice.
- libp2p is confined to this package. Measure RSS and goroutines; the Pi Zero 2 W target is real.
- JSON Lines on `/pigeon/0/store`; envelopes are base64 and are never re-encoded.
- Spike stubs, to be removed in later milestones: the inbox member set is static (`Config.InboxMembers`) instead of DHT inbox records. `Config.Devices` and the loopback admin HTTP interface (`admin.go`) inject devices and messages by hand for testing without a gateway.
- `roost_test.go` runs the three-node spike topology in one process. It needs the network stack and takes about 10 s; `go test -short` skips it.
