# roost package

The daemon. Implements `docs/pigeon-node-protocol.md` on go-libp2p plus the M17_inet proxy side described in `docs/pigeon-architecture.md` §2.

- Two faces: M17_inet (UDP, reflector-compatible) toward gateways and clients; libp2p toward other nodes. One flat package: keep the faces in separate files (`inet.go`, `p2p.go`) with a narrow interface between them, but no sub-packages, which invite import cycles over shared types.
- Room subscription state (rooms spec §4: latest ROOM envelope per room wins, sticky opt-outs, expiry) lives here in `rooms.go`, not in its own package.
- Proxy side: per-reflector local ports and a generated hosts file so an unmodified gateway keeps its normal voice reflector behaviour. Stream frames and unhandled packet types pass through untouched; only MSG, RCPT, ROOM, and SMS are intercepted.
- Presence is published truthfully with `via` (RF, local client, internet client). Never fabricate presence.
- Homing a callsign: publish presence → read or create inbox record → WATCH members → sweep → replay within the replay window for messages without DELIVERED. See node protocol §7.
- Only the loft writes inbox records. Repair requires a member to be both unreachable across three sweeps and silent in presence for the failure period. Log every record write with old and new versions.
- Never deliver the same message ID to the same device twice.
- libp2p is confined to this package. Measure RSS and goroutines; the Pi Zero 2 W target is real.
- JSON Lines on `/pigeon/0/store`; envelopes are base64 and are never re-encoded.
- Spike stubs, to be removed in later milestones: the inbox member set is static (`Config.InboxMembers`) instead of DHT inbox records; there is no M17_inet face yet, so local devices come from `Config.Devices` and messages enter through `Roost.Send` / `Roost.HandleRoom`.
- `roost_test.go` runs the three-node spike topology in one process. It needs the network stack and takes about 10 s; `go test -short` skips it.
