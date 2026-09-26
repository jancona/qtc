# qtcd package

The daemon. Implements `docs/qtc-node-protocol.md` on go-libp2p plus the M17_inet proxy side described in `docs/qtc-architecture.md` §2.

- Two faces: M17_inet (UDP, reflector-compatible) toward gateways and clients; libp2p toward other nodes. One flat package: keep the faces in separate files (`inet.go`, `p2p.go`) with a narrow interface between them, but no sub-packages, which invite import cycles over shared types.
- Room subscription state (rooms spec §4: latest ROOM envelope per room wins, sticky opt-outs, expiry) lives here in `rooms.go`, not in its own package.
- Client face (`inet.go`, node protocol §10): the station is one M17_inet reflector (e.g. `M17-QTC`) on one UDP port. Each module letter maps to an upstream reflector+module and a mode: `native` is a pure proxy, `qtc` takes SMS/MSG/RCPT/ROOM into QTC and never forwards them, and drops messaging arriving from upstream. Unmapped modules get NACK. Voice always passes through untouched. Framing, LSF, and CRC come from the `m17` root package (v0.6.0 and later is protocol-only and stdlib-only) through the thin adapters in `m17frame.go`; never import `m17/modem`, `m17/inet`, or `m17/dashboard` here.
- Presence is published truthfully with `via` (RF, local client, internet client). Never fabricate presence.
- Homing a callsign: publish presence → read or create mailbox record → WATCH members → sweep → replay within the replay window for messages without DELIVERED. See node protocol §7.
- Only the home station writes mailbox records. Repair requires a member to be both unreachable across three sweeps and silent in presence for the failure period. Log every record write with old and new versions.
- Never deliver the same message ID to the same device twice.
- libp2p is confined to this package. Measure RSS and goroutines; the Pi Zero 2 W target is real.
- JSON Lines on `/qtc/0/store`; envelopes are base64 and are never re-encoded.
- Mailbox records (`record.go`, `records.go`, node protocol §4, §7.3, §8): signed JSON in the DHT under `/qtc/0/mailbox/<BASE>` with a namespaced validator, announced on `/qtc/0/mailbox-records`, cached with monotonic versions. The first station to home a callsign creates its record; a sender creates a provisional one that the first station to hear the callsign takes over; the home station repairs failed members on its sweep and copies the full history to the recruit. `Config.MailboxMembers` are only seeds for record creation when presence knows no mailbox-capable station. Migration (§8.3) is not implemented.
- Persistence (`Config.DataDir`): the node's own mailbox is a `store.FileStore` and the delivered-once table (`delivered.go`) is journaled, both as JSON Lines under the data dir, opened in `Start` (never in `New`, so `-print-id` creates only the key). A mailbox PUT is acknowledged only after it is fsynced to the journal. Delivered-once entries are kept for the longest possible envelope TTL, since a repair can re-offer any message a mailbox still holds. Empty `DataDir` keeps everything in memory (tests).
- `InetConfig.AllowCallsigns` limits internet clients (outside `Gateways`) by base callsign, on CONN and on every packet source in a qtc module. It keeps strangers out; it is not authentication.
- `Config.Devices` and the loopback admin HTTP interface (`admin.go`) inject devices and messages by hand for testing without a gateway.
- `station_test.go` runs the three-node spike topology in one process. It needs the network stack and takes about 10 s; `go test -short` skips it.
