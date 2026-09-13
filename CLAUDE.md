# Pigeon: An M17 Messaging System

Store-and-forward text messaging for M17 amateur radio, by callsign and by room.
Go, Apache 2.0. Maintainer: Jim, N1ADJ.

## Read first

The design lives in `docs/`. Read `pigeon-architecture.md` before any non-trivial change; it defines the vocabulary (roost, public node, inbox, loft, homing, base callsign, device, clock-in). Use those terms exactly. The other specs (`pigeon-envelope.md`, `pigeon-rooms.md`, `pigeon-node-protocol.md`) cover the layer you are working in. `pigeon-node-protocol-draft01-custody.md` is superseded and kept only as history.

`docs/pigeon-fixtures.json` is the test-vector file, produced by the independent reference implementation `docs/pigeon-fixtures-gen.py`. Go code must reproduce every fixture exactly, except signature bytes, which it must verify. If a fixture looks wrong, fix the generator and the spec together, never just the Go test.

## Decisions not to relitigate

Reasoning is in the docs. Do not "simplify" these away.

- Message IDs are derived (SHA-256 of canonical fields, 8 bytes), never transmitted. The envelope is immutable end to end.
- Envelopes are opaque bytes everywhere (base64 in JSON, raw on RF). Parsed for indexing, never rebuilt from a struct.
- Declared inboxes, not custody transfer. Senders write to every inbox member and retry until all accept.
- Inbox nodes never talk to each other. Reconciliation is the homing node's sweep.
- Only the loft writes a callsign's inbox record (plus silent-period takeover and provisional handoff). Convention, not DHT-enforced; known gap awaiting user keys.
- Node keys now (ECDSA secp256r1), user keys later. Do not invent a user identity scheme.
- No `@ALL`. Node-callsign rooms replace it. Explicit LEAVE is sticky.
- Legacy SMS radios are terminals: no sync, no dedup. Invest in the native envelope path instead.
- No confidentiality. TLS authenticates nodes; content is plaintext. Never say "encrypted".
- Packet types `0x08` MSG, `0x09` RCPT, `0x0A` ROOM are provisional; `0x07` is TLE in M17 3.0.0 and must not be used. Keep them as named constants in one place.

## Layout

```
envelope/   MSG/RCPT/ROOM parsing, IDs, signatures, addresses and room name encoding. stdlib only.
store/      JSON Lines put/query/watch protocol, client and server. stdlib + envelope.
roost/      the daemon as a library, one flat package: subscriptions, M17_inet proxy side, libp2p side, homing, sweeps, loft.
cmd/roost/  daemon binary: config, flags, signals, wiring.   cmd/pigeon/  CLI (decode, keys, fixtures, store client).
docs/       specs and fixtures.
```

Imports go one way: `envelope` → `store` → `roost` → `cmd`. Few packages on purpose: a new package needs a consumer that must not import what it would otherwise pull in (the CLI must not link libp2p, which is why `store` is separate). Split by file, not by package, until then. Anything that wants to import the other way is a design problem, not a packaging one.

Depends on `github.com/jancona/m17`, checked out alongside and resolved via `go.work` in the parent directory. Protocol-level M17 work (framing, addresses, M17_inet) goes in `m17`; Pigeon-specific code goes here. When in doubt, ask.

## Conventions

- Go 1.22+. `gofmt`, `go vet`, `staticcheck` clean. Table-driven tests; every encoder has a fixtures test.
- Wrap errors with context; no panics outside `main`. Log with `log/slog`.
- Wire formats are big-endian. Timestamps are `uint32` Unix seconds; `0` means unknown.
- No hand-implemented cryptographic primitives. Use `crypto/*` from the standard library; if it cannot do what a spec asks, stop and ask rather than writing it. Interop tests verify signatures; they never compare signature bytes.
- libp2p is the one large dependency and lives only under `roost/`.
- Target includes Raspberry Pi Zero 2 W (arm64, 512 MB). Watch memory and goroutine counts.

## Current milestone

Spike: two roosts behind NAT on separate home networks plus one public node on a VPS. Presence over gossipsub, one room topic, one inbox with put/query/watch. Success: runs alongside a normal hotspot install (gateway + dashboard already at load ~1.2, ~200 MB) on a Pi Zero 2 W without visibly degrading it; RSS and idle CPU recorded in `docs/spike-results.md`.

Order: `envelope` against fixtures → `store` → `roost` skeleton (subscriptions first) → spike.

## Open questions (do not silently resolve)

Presence establishment on the radio side; thin-client transport; inbox quotas; DHT validator vs. loft convention; backlog on RF. If work touches one, record the choice in that spec's Open Questions section.
