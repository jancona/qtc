# QTC operator guide

This guide is for people running an M17 hotspot who want to add QTC to it. It covers installing the `qtcd` package, connecting it to your gateway, checking that it works, configuration, upgrades, and troubleshooting. For using QTC from a radio, see the [user guide](user-guide.md).

QTC is in an **invite-only test**. Before you install it, read [known limitations](#known-limitations): messages are not private, and callsigns are not verified.

## What qtcd does on a hotspot

`qtcd` is a small service that runs next to `m17-gateway` on your Pi. Your gateway links to it as if it were a reflector named `M17-QTC`, module A. qtcd passes voice straight through to the reflector and module your gateway used before, so voice works as it always did. Text messages (M17 SMS) are different: qtcd takes them into the QTC network, which stores them and forwards them to the recipient's hotspot, wherever that is.

```
radio ── RF ── m17-gateway ── M17-QTC module A (qtcd, on the same Pi)
                                  ├── voice ──► your usual reflector, e.g. M17-M17 module C
                                  └── SMS ────► QTC network (store and forward)
```

On a hotspot, qtcd:

- Makes outgoing connections only. You don't need to open any ports on your router.
- Listens for the gateway on `127.0.0.1:17000`, so only this Pi can reach it.
- Uses about 15 MB of memory and almost no CPU. It was tested alongside a full hotspot install on a Pi Zero 2 W.
- Doesn't store other people's messages. That's the job of public stations with the `mailbox` capability.

## Requirements

- A Raspberry Pi running Raspberry Pi OS Bookworm or Trixie, 64-bit (`arm64`) or 32-bit (`armhf`), with `m17-gateway` installed. The [M17 hotspot installer](https://github.com/M17-Project/m17-hotspot-installer) sets that up.
- An internet connection that allows outgoing TCP.
- A correct clock. Messages carry timestamps, and expiry and redelivery depend on them. Raspberry Pi OS keeps time with `systemd-timesyncd` by default; check with `timedatectl`.

## Installing

### With the hotspot installer (recommended)

Until the change is merged into the M17 Project's installer, it's on a branch in a fork. On the Pi:

```
wget https://raw.githubusercontent.com/jancona/m17-hotspot-installer/refs/heads/qtc/m17-hotspot-installer.sh
chmod u+x m17-hotspot-installer.sh
sudo ./m17-hotspot-installer.sh -q
```

`-q` installs QTC without asking. Without it, the installer asks near the end. Add `-n` to skip flashing modem firmware on a hotspot that's already set up. The installer is safe to run on an existing hotspot, but it does run a full system upgrade and update the dashboard.

The QTC step:

1. Downloads the newest `qtcd` package for your Pi's architecture from [the QTC releases](https://github.com/jancona/qtc/releases).
2. Installs it, passing your gateway's callsign to it.
3. Adds `M17-QTC 127.0.0.1 17000` to `/opt/m17/rpi-dashboard/files/OverrideHosts.txt`, so the gateway can find qtcd.

It doesn't change which reflector your gateway uses. That's the next step: [connecting the gateway](#connecting-the-gateway).

### By hand

Download the `.deb` for your Pi from the [releases page](https://github.com/jancona/qtc/releases). Run `dpkg --print-architecture` to see which one you need: `arm64` or `armhf`. Then:

```
sudo apt install ./qtcd_0.1.0.rc2_arm64.deb
echo "M17-QTC 127.0.0.1 17000" | sudo tee -a /opt/m17/rpi-dashboard/files/OverrideHosts.txt
```

### What the package sets up

On a new install, the package writes `/etc/qtcd.ini` from your gateway's settings:

| Setting | Taken from | Example |
|---|---|---|
| Node callsign | your gateway's callsign, base only, with module letter `Q` in the ninth position | gateway `N1ADJ C` → node `N1ADJ   Q` |
| Upstream for voice | your gateway's current reflector and module | `M17-M17`, module `C` |
| Hosts file | the dashboard's `M17Hosts.txt` | `/opt/m17/rpi-dashboard/files/M17Hosts.txt` |

The package then creates the node key, enables and starts the `qtcd` service, and prints the node's peer ID and where voice will go. If the gateway has no callsign yet, and you're installing from a terminal, it asks for one. If you leave it blank, the config keeps its placeholders and qtcd doesn't start until you fill them in (see [configuration](#configuration)).

The package never changes an existing `/etc/qtcd.ini`.

## Connecting the gateway

In the dashboard, open **Gateway Config** and set:

- **Reflector:** `M17-QTC`
- **Module:** `A`

Save and let the gateway restart. From then on, voice still reaches the reflector and module shown in `/etc/qtcd.ini` (`[Module A]`), and SMS goes into QTC.

To go back, set the reflector and module to their old values. qtcd can keep running; it does nothing while nothing links to it.

## Checking that it works

```
systemctl status qtcd
journalctl -u qtcd -f
```

Soon after qtcd starts, you should see these lines:

```
level=INFO msg="station started" node="N1ADJ   Q" id=QmUt8Zff… …
level=INFO msg="connected to bootstrap peer" … peer=QmdxViFZ…
level=INFO msg="client linking" … callsign="N1ADJ   C" module=A upstream=107.191.121.105:17000 …
level=INFO msg="upstream linked" …
level=INFO msg="new node" … callsign="N1ADJ  P" caps=7
```

- `connected to bootstrap peer`: qtcd reached the test network's public station.
- `client linking` and `upstream linked`: your gateway linked to qtcd, and qtcd linked to your usual reflector.
- `new node`: qtcd has heard another node's presence. This can take a few minutes.

`journalctl -u qtcd -p warning` shows only warnings and errors.

The status page gives the node's view of the network:

```
curl -s localhost:8017/status
```

| Field | Meaning |
|---|---|
| `id`, `callsign` | This node's peer ID and callsign |
| `peers` | Nodes connected right now. An address with `/p2p-circuit` means the connection is relayed. |
| `presence` | Nodes heard, and the radios each one has heard |
| `homed` | Callsigns this node is home station for, meaning radios heard through this hotspot, with their rooms |
| `records` | Mailbox records this node has looked up |
| `rooms` | Rooms with a subscriber on this node |

A radio is heard when it transmits through the hotspot, whether voice or SMS. Its callsign then appears under `homed`, and messages for it are delivered here.

For a quick end-to-end test from a radio, send the SMS `/rooms` to the node's callsign (`N1ADJ Q`; one space is fine). The node replies by SMS with the rooms you're in, typically `rooms: N1ADJ`: every radio heard on a node is subscribed to that node's local room, which is named after the node's base callsign.

## Configuration

The config file is `/etc/qtcd.ini`. It's in the same style as `/etc/m17-gateway.ini`:

```ini
[General]
; This node's callsign: your base callsign, module letter Q ninth.
Callsign=N1ADJ   Q
DataDir=/var/lib/qtcd
Admin=127.0.0.1:8017
MetricsInterval=15m

[Network]
Bootstrap=/dns4/ham.n1adj.net/tcp/4001/p2p/QmdxViFZP4PK5TL3xANxScyaFnvBfdqfbQqjjJGhvJvuSg

[Inet]
Listen=127.0.0.1:17000
HostsFile=/opt/m17/rpi-dashboard/files/M17Hosts.txt

[Module A]
Reflector=M17-M17
Module=C
Mode=qtc
```

After editing, run `sudo systemctl restart qtcd`. If qtcd finds a mistake, it doesn't start, and the journal names the file, section, and setting:

```
level=ERROR msg=config err="/etc/qtcd.ini: [Network]: unknown setting bootsrap"
```

Settings you might change:

| Setting | When to change it |
|---|---|
| `[Module A] Reflector`, `Module` | To send voice to a different reflector or module. `Reflector` is a name from the hosts file, or `host:port`. |
| `[General] Callsign` | Only if the generated one is wrong. Each node needs its own callsign, distinct from your radios'. Pad the base callsign to eight characters and put the module letter ninth. |
| `[Inet] HostsFile` | If your `M17Hosts.txt` is somewhere other than the dashboard's location. |
| `[Module B]` and so on | To offer more modules. `Mode=native` makes a plain pass-through where SMS goes upstream too. |

[`config.example.ini`](../cmd/qtcd/config.example.ini) describes every setting and its default. Sections and setting names aren't case-sensitive, and lists are comma-separated.

## Files

| Path | What it is |
|---|---|
| `/etc/qtcd.ini` | Configuration |
| `/var/lib/qtcd/node.key` | The node's identity. Its peer ID is derived from it. Keep it: a new key makes a new node. |
| `/var/lib/qtcd/delivered.jsonl` | Which messages have gone to which radio, so a restart never delivers one twice |
| `/var/lib/qtcd/mailbox.jsonl` | Stored messages, on public stations with the `mailbox` capability only |
| `/usr/lib/systemd/system/qtcd.service` | The service. It runs as user `qtcd` and can write only to `/var/lib/qtcd`. |

## Upgrading and removing

Upgrade with the installer, or install a newer `.deb` over the old one. The service restarts; your config and node key are kept.

- **From 0.1.0-rc1:** rc1 used `/etc/qtcd.json`. The upgrade renames it to `/etc/qtcd.json.rc1` and writes a new `/etc/qtcd.ini` from your gateway's settings, keeping the node key. Check that `[Module A]` names the reflector you want.
- **Remove:** `sudo apt remove qtcd` stops and removes the service but keeps `/etc/qtcd.ini` and `/var/lib/qtcd`.
- **Purge:** `sudo apt purge qtcd` also deletes the config, the node key, and the `qtcd` user.

Before removing qtcd, point the gateway back at your usual reflector, and remove the `M17-QTC` line from `OverrideHosts.txt` if you like.

## Troubleshooting

**The gateway doesn't link to M17-QTC.** Check that `OverrideHosts.txt` has `M17-QTC 127.0.0.1 17000` and the gateway config says `M17-QTC`, module `A`. Look for `client linking` in `journalctl -u qtcd`. If it's missing, the gateway isn't reaching qtcd; `journalctl -u m17-gateway` shows why.

**The gateway links but voice doesn't reach the reflector.** Look for `upstream linked`. If it never appears, or qtcd logs `upstream silent; relinking`, check that `[Module A] Reflector` is spelled as it appears in `M17Hosts.txt`, and that the reflector is up.

**qtcd won't start.** Run `journalctl -u qtcd -n 20`. A config error names the setting. `address already in use` on port 17000 means something else on the Pi is listening there. `bind: address already in use` on 8017 means another program is using the admin port; change `[General] Admin`.

**No `connected to bootstrap peer`.** qtcd can't reach `ham.n1adj.net` on TCP port 4001. Check the Pi's internet connection and any outgoing firewall. qtcd keeps retrying on its own.

**A radio's messages aren't delivered.** qtcd sends to a radio only if it was heard (transmitted, by voice or SMS) through this hotspot within the last hour. Otherwise it holds messages until the radio is next heard, then sends the 10 most recent, oldest first, with a note saying how many older ones it left out. So a radio that has been listening quietly for over an hour gets its messages when it next keys up. `journalctl -u qtcd | grep -E "holding|replay"` shows this happening. The hour is `[Delivery] ReachWindow`.

**Reporting a problem.** Open an issue at [github.com/jancona/qtc/issues](https://github.com/jancona/qtc/issues) and include:

- `qtcd -version`
- the output of `curl -s localhost:8017/status`
- the relevant part of `journalctl -u qtcd`

For more detail, turn on debug logging with `sudo systemctl edit qtcd`, add these lines, and restart. Remove them afterwards; debug logging is verbose.

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/qtcd -config /etc/qtcd.ini -log-level debug
```

## Running a public station

Most testers run hotspots. A public station stores mailboxes for others, relays for nodes behind NAT, and can accept internet-only clients. It needs a stable public address with inbound ports open. Differences from a hotspot config:

```ini
[Network]
Listen=/ip4/0.0.0.0/tcp/4001
Caps=public,relay,mailbox

[Inet]
; Accept gateways and clients from the internet, not just this machine
Listen=0.0.0.0:17000
; Only these callsigns may connect as internet clients
AllowCallsigns=N1ADJ,W1AW
HostsFile=/path/to/M17Hosts.txt

[Module A]
Reflector=M17-M17
Module=C
Mode=qtc
```

Forward TCP 4001 (libp2p) and, for internet-only clients, UDP 17000 to the station. `AllowCallsigns` keeps strangers off an open client face, but it doesn't verify anyone's identity. Gateways on the address ranges in `[Inet] Gateways` (default: private ranges) aren't limited. Other nodes use the station by adding its address to `[Network] Bootstrap`: `/dns4/<name>/tcp/4001/p2p/<peer ID>`. `qtcd -config /etc/qtcd.ini -print-id` prints the peer ID.

## Known limitations

- **Messages are not private.** Nodes authenticate each other, but message content is plain text to every node that stores or forwards it, and amateur radio rules don't allow encryption anyway.
- **Callsigns are not verified.** A radio or client says who it is, and QTC believes it. Anyone could send a message that claims to be from you, or connect claiming your callsign. `AllowCallsigns` on a public station limits who can try, not who they are.
- **Nodes are trusted.** A misbehaving node could overwrite the record of which stations hold a callsign's mailbox. There are no quotas yet either. This is why the test is invite-only.
- **The protocol is a draft.** During the test, run the latest release. Releases may not work with older ones.
- **Radios have to transmit to be reachable.** qtcd sends messages to a radio only if it was heard within the last hour, and holds them otherwise. When the radio is next heard, it gets at most the 10 most recent, plus a note saying how many older ones were left out. See the [user guide](user-guide.md).
