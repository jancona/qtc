# QTC: An M17 Messaging System

Store-and-forward text messaging for M17 amateur radio, by callsign and by room.
Go, Apache 2.0. Maintainer: Jim, N1ADJ.

## Read first

The design lives in `docs/`. Read `qtc-architecture.md` before any non-trivial change; it defines the vocabulary (qtcd, station, public station, mailbox, home station, homing, base callsign, device, QSL). Use those terms exactly. The other specs (`qtc-envelope.md`, `qtc-rooms.md`, `qtc-node-protocol.md`) cover the layer you are working in. `qtc-node-protocol-draft01-custody.md` is superseded and kept only as history.

`docs/qtc-fixtures.json` is the test-vector file, produced by the independent reference implementation `docs/qtc-fixtures-gen.py`. Go code must reproduce every fixture exactly, except signature bytes, which it must verify. If a fixture looks wrong, fix the generator and the spec together, never just the Go test.

## Decisions not to relitigate

Reasoning is in the docs. Do not "simplify" these away.

- Message IDs are derived (SHA-256 of canonical fields, 8 bytes), never transmitted. The envelope is immutable end to end.
- Envelopes are opaque bytes everywhere (base64 in JSON, raw on RF). Parsed for indexing, never rebuilt from a struct.
- Declared mailboxes, not custody transfer. Senders write to every mailbox member and retry until all accept.
- Mailbox nodes never talk to each other. Reconciliation is the homing node's sweep.
- Only the home station writes a callsign's mailbox record (plus silent-period takeover and provisional handoff). Convention, not DHT-enforced; known gap awaiting user keys.
- Node keys now (ECDSA secp256r1), user keys later. Do not invent a user identity scheme.
- No `@ALL`. Node-callsign rooms replace it. Explicit LEAVE is sticky.
- Legacy SMS radios are terminals: no sync. Invest in the native envelope path instead. The one exception, decided 2026-09-27: stations share delivery records through the recipient's mailbox (node protocol §7.6), so a radio moving between hotspots is not replayed messages it already had. Keep that mechanism minimal.
- No confidentiality. TLS authenticates nodes; content is plaintext. Never say "encrypted".
- QTC is one M17 packet type, `0x08` (provisional), with a Kind byte after it: MSG, RCPT, ROOM, SYNC, ACK (`envelope/types.go`). Don't ask the working group for more packet types; add kinds. `0x07` is TLE in M17 3.0.0 and must not be used.
- Signatures cover `"QTC" ‖ Kind ‖` the signed fields; mailbox records sign under `"QTC-record"`. Message IDs exclude Type and Kind.

## Layout

```
envelope/   QTC packet kinds (MSG/RCPT/ROOM/SYNC/ACK): parsing, IDs, signatures, addresses and room name encoding. stdlib only.
store/      JSON Lines put/query/watch protocol, client and server. stdlib + envelope.
qtcd/       the daemon as a library, one flat package: subscriptions, M17_inet proxy side, libp2p side, homing, sweeps, home station.
cmd/qtcd/   daemon binary: config, flags, signals, wiring.   cmd/qtc/  CLI (decode, keys, fixtures, store client).
cmd/qtcd/packaging/  .deb for qtcd + qtc (systemd unit, maintainer scripts, sample config); scripts/build-deb.sh VERSION ARCH. CI (.github/workflows/qtc.yml) releases it on v* tags.
app/        GUI client (Fyne, desktop and Android). Its own Go module, listed in go.work.
docs/       specs and fixtures.
```

`app/` is a separate module so Fyne never enters the daemon's or CLI's dependency graph. It may import `envelope`, `store` and `github.com/jancona/m17`; nothing in the main module imports it. Release builds run with `GOWORK=off` against the tagged `m17` in `go.mod`, so any `m17` change a release needs must be tagged first.

Imports go one way: `envelope` → `store` → `station` → `cmd`. Few packages on purpose: a new package needs a consumer that must not import what it would otherwise pull in (the CLI must not link libp2p, which is why `store` is separate). Split by file, not by package, until then. Anything that wants to import the other way is a design problem, not a packaging one.

Depends on `github.com/jancona/m17`, checked out alongside and resolved via `go.work` in the parent directory. Protocol-level M17 work (framing, addresses, M17_inet) goes in `m17`; QTC-specific code goes here. When in doubt, ask.

## Conventions

- Go 1.26 (per `go.mod`). `gofmt`, `go vet`, `staticcheck` clean. Table-driven tests; every encoder has a fixtures test.
- Wrap errors with context; no panics outside `main`. Log with `log/slog`.
- Wire formats are big-endian. Timestamps are `uint32` Unix seconds; `0` means unknown.
- No hand-implemented cryptographic primitives. Use `crypto/*` from the standard library; if it cannot do what a spec asks, stop and ask rather than writing it. Interop tests verify signatures; they never compare signature bytes.
- libp2p is the one large dependency and lives only under `qtcd/`.
- Target includes Raspberry Pi Zero 2 W (arm64, 512 MB). Watch memory and goroutine counts.

## Current milestone

Milestones 1–4 are done (`docs/qtc-architecture.md` §9, results in `docs/spike-results.md`). Now: milestone 5, an invite-only test with known hotspot operators and internet-only users. Exit criteria are in §9.

Done: persistence, the `[Inet] AllowCallsigns` allowlist, the `.deb` and release CI, the hotspot-installer fork (jancona/m17-hotspot-installer, branch `qtc`), INI config (`/etc/qtcd.ini`, parsed in `cmd/qtcd/config.go` with `gopkg.in/ini.v1` like m17-gateway; unknown keys are errors). Next: operator guide → `qtc chat` and the `app/` GUI (desktop and Android) → user guide → onboarding with ham.n1adj.net as the public station. Quotas and user keys stay deferred; don't pull them in.

Every change must still run alongside a normal hotspot install on a Pi Zero 2 W without visibly degrading it.

## Open questions (do not silently resolve)

Presence establishment on the radio side; thin-client transport; mailbox quotas; DHT validator vs. home station convention; backlog on RF. If work touches one, record the choice in that spec's Open Questions section.
