# QTC: store-and-forward messaging for M17

QTC lets M17 hams send text messages to each other by callsign, and have them arrive even when the other station isn't on the air right now. It runs alongside an ordinary M17 hotspot, works with the radios you already have, and leaves voice alone.

The name is the Q-code: *QTC* means "I have messages for you."

> **Status: experimental, invite-only test.** Messages are not private, callsigns are not verified, and things may change between releases. See [what to keep in mind](#what-to-keep-in-mind).

## Why

M17 radios can already send text messages (SMS), but only in the moment. A message goes to the reflector you're linked to, and it reaches the other station only if they're linked to the same reflector and module, and listening, at that instant. If they're off the air, on another reflector, or out of range, the message is gone, and you'll never know.

QTC turns that into messaging you can rely on:

- **Send by callsign, not by reflector.** Write to `W1XYZ`, and QTC finds the hotspot that last heard W1XYZ, wherever it is.
- **Messages wait for you.** If you're off the air, your messages are held for up to a week (or as long as the sender chose) and delivered when you're next heard: after a key-up, or when you connect from a computer.
- **They follow you.** Messages go to whichever QTC hotspot hears you, and one you've already received isn't sent again when you move to another hotspot.
- **Rooms.** Join a named group conversation such as `#MAINE` or `#NET` from your radio, and everyone in it gets the posts, on whatever QTC hotspot they're using. Each hotspot also has a local room for everyone it has heard.
- **From a computer, too.** `qtc chat` connects to a QTC station over the internet, so you can message radio users from your desk, and catch up on what you missed whenever you connect. An app for desktop and Android is planned.
- **No central server.** QTC is a network of nodes run by hams. Every hotspot running QTC is a node, and public stations store messages for others. Anyone can run any part of it.

## How it works

A QTC hotspot is a normal M17 hotspot running one extra service, `qtcd`. The hotspot's gateway links to `qtcd` as though it were a reflector named `M17-QTC`. Voice passes straight through to the reflector and module you used before. Text messages go into the QTC network instead.

```
your radio ── RF ── hotspot gateway ── qtcd (M17-QTC)
                                         ├── voice ──► your usual reflector, as before
                                         └── SMS ────► the QTC network
                                                       stores messages and delivers them
                                                       through the hotspot that hears
                                                       the recipient
```

Behind that, nodes find each other over the internet and keep each callsign's messages on a few public stations, its *mailbox*. When a hotspot hears your radio, it fetches what's waiting for you and transmits it.

QTC works with today's M17 radios: they send and receive ordinary SMS, and QTC does the rest. QTC also defines its own packet format for radios and programs that support it. These get delivery confirmations, retries over a weak signal, and catch-up on messages they missed. `qtc chat` uses it today; radio firmware with QTC support doesn't exist yet.

## What to keep in mind

- **Messages are not private.** Every station that stores or forwards a message can read it, and amateur radio rules don't allow encryption anyway. Don't send anything you wouldn't say on the air.
- **Callsigns are not verified.** QTC believes the callsign a radio or client gives, so a message that claims to be from someone might not be. Public stations can limit who connects, but that keeps strangers out; it doesn't prove identity.
- **This is a test.** The protocol is a draft. Run the latest release, expect occasional lost messages, and please report problems.

## Trying it

QTC is in an invite-only test with a small group of hams. If you'd like to take part, find N1ADJ on the [M17 Project Discord](https://discord.gg/4brEP8wwVp), or open an issue at [github.com/jancona/qtc/issues](https://github.com/jancona/qtc/issues).

- **You run an M17 hotspot:** the [operator guide](docs/operator-guide.md) covers installing `qtcd` on a Raspberry Pi hotspot running `m17-gateway`, with the hotspot installer or the `.deb` package.
- **You use an M17 radio:** the [user guide](docs/user-guide.md) explains sending messages, rooms, and what happens while you're away.
- **You don't have a radio:** `qtc chat` runs on macOS, Windows, and Linux. The [user guide](docs/user-guide.md#from-a-computer-qtc-chat) explains how to connect.

Downloads are on the [releases page](https://github.com/jancona/qtc/releases).

## For developers

QTC is written in Go. The design is in [`docs/`](docs/README.md): start with the architecture overview, then the specifications for the message envelope, rooms, the node protocol, and native clients. `docs/qtc-fixtures.json` has test vectors from an independent Python implementation.

```
go build ./...    # qtcd (the node), qtc (the CLI, including qtc chat), qtc-radio
go test ./...
```

| Directory | What's there |
|---|---|
| `envelope/` | The QTC packet format: messages, receipts, room control, sync |
| `store/` | The mailbox storage protocol |
| `qtcd/` | The node: the hotspot-facing side, the network side, delivery |
| `client/` | The client side of the native protocol, shared by `qtc chat` and `qtc-radio` |
| `cmd/` | The `qtcd`, `qtc`, and `qtc-radio` programs, and packaging |

M17 framing and the hotspot software come from [jancona/m17](https://github.com/jancona/m17). `qtc-radio`, a test tool that turns a spare hotspot modem into a radio that speaks QTC, is described in [`docs/testbed.md`](docs/testbed.md).

## License

Apache 2.0. Maintained by Jim Ancona, N1ADJ.
