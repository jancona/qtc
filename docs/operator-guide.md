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
- m17-gateway 0.6.2 or later.
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
sudo apt install ./qtcd_0.1.0.rc4_arm64.deb
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

A radio is heard when it transmits through the hotspot, whether voice or SMS. Transmissions to ECHO or INFO don't count: the gateway answers those itself, and qtcd never sees them. Its callsign then appears under `homed`, and messages for it are delivered here.

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
| `[Module A] Reflector`, `Module` | To send voice to a different reflector or module. `Reflector` is a name from the hosts file, or `host:port`. Leave both out for a messaging-only module, with no voice. |
| `[General] Callsign` | Only if the generated one is wrong. Each node needs its own callsign, distinct from your radios'. Pad the base callsign to eight characters and put the module letter ninth. |
| `[Inet] HostsFile` | If your `M17Hosts.txt` is somewhere other than the dashboard's location. qtcd reads it again daily (`[Inet] HostsRefresh`). Without `HostsFile`, qtcd downloads the M17 Project's list daily instead. |
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

- **Remove:** `sudo apt remove qtcd` stops and removes the service but keeps `/etc/qtcd.ini` and `/var/lib/qtcd`.
- **Purge:** `sudo apt purge qtcd` also deletes the config, the node key, and the `qtcd` user.

Before removing qtcd, point the gateway back at your usual reflector, and remove the `M17-QTC` line from `OverrideHosts.txt` if you like.

## Getting your hotspot on frequency

Your hotspot's transmit frequency matters more for text messages than for voice. M17 uses four-level FSK, and the two inner levels are only about 1.6 kHz apart, so a frequency error of a few hundred Hz eats most of the margin. A receiver without automatic frequency correction, such as an MMDVM hotspot, can go from decoding every packet to decoding none when the transmitter is 400 Hz off. Some radios tolerate more, but SMS is single-shot: a packet that doesn't decode is lost.

Hotspot crystals are commonly off by several hundred Hz. In testing, one CC1200 hotspot transmitted 318 Hz low and an MMDVM hotspot listening to it was set 300 Hz high. Before correction the MMDVM decoded 7 of 20 packets; with both corrected it decoded 60 of 60, and 0 of 30 once the CC1200 was moved 400 Hz off again. So it's worth measuring yours.

### What you need

- An RTL-SDR with a temperature-compensated crystal. The RTL-SDR Blog V3 and V4 both work.
- SDR software with an M17 decoder that shows the symbol constellation, such as [SDR++](https://www.sdrpp.org/).
- [LTE-Cell-Scanner](https://github.com/JiaoXianjun/LTE-Cell-Scanner), to calibrate the SDR against LTE cell towers. Their carriers are GPS-disciplined, so they make an accurate reference that's available almost everywhere.

A crystal's error is the same number of parts per million (ppm) at any frequency. So calibrating the SDR at an LTE frequency tells you its error at 440 MHz too: 1 ppm at 444 MHz is 444 Hz.

### 1. Calibrate the SDR

Let the dongle run for 15 to 20 minutes first, and keep it running while you measure. Then scan a lower LTE band, where the V3's tuner is happiest:

```
CellSearch -s 729e6 -e 756e6 -p 5
```

For 850 MHz, use `-s 869e6 -e 894e6`. Each cell found is listed with a `freq-offset` in Hz and a `CrystalCorrectionFactor`. Your dongle's error in ppm is the offset divided by the cell's frequency: for example, −678 Hz at 731.5 MHz is −0.93 ppm. Average the cells, and run the scan two or three times to check the results agree.

A negative offset means your dongle reads signals **low**. Leave your SDR program's ppm correction at 0, and instead add the error to every reading by hand. At 444 MHz, a dongle at −1.04 ppm reads 462 Hz low, so add 462 Hz.

LTE-Cell-Scanner builds on Linux from its instructions. On macOS it needs `brew install itpp fftw librtlsdr boost cmake` and two edits to its `CMakeLists.txt`: remove `system` from `FIND_PACKAGE( Boost COMPONENTS thread system REQUIRED )`, and change `-std=c++11` to `-std=c++17`. Then configure with `-DUSE_OPENCL=0 -DCMAKE_POLICY_VERSION_MINIMUM=3.5`, and give it the Homebrew paths: `-DITPP_INCLUDE_DIR`, `-DITPP_LIBRARY_NORMAL`, `-DFFTW_INCLUDE_DIR`, `-DFFTW_LIBRARY`, `-DRTLSDR_INCLUDE_DIR` and `-DRTLSDR_LIBRARY`, each pointing under `/opt/homebrew/opt/`.

### 2. Measure the hotspot

Set `FrequencyCorr = 0` in `/etc/m17-gateway.ini` and restart m17-gateway. Key up your radio to the **ECHO** destination: the hotspot records your transmission and plays it back, which gives you several seconds of the hotspot's own signal.

- **Using the M17 decoder:** tune the SDR until the constellation's dots sit on its lines, and read that frequency. Changes of less than about 100 Hz are hard to see, so treat the result as ±100 Hz.
- **Using a carrier:** some hotspots leave a short unmodulated carrier at the end of each transmission (the SX1255 does). Its peak can be read more precisely, with a narrow FFT and some averaging.

Add your dongle's correction to the reading. The difference from the nominal frequency is the hotspot's error.

### 3. Correct it

Set `FrequencyCorr` in the `[Radio]` section of `/etc/m17-gateway.ini` to cancel the error, then restart m17-gateway. `FrequencyCorr` shifts receive and transmit together, which is what you want, since both come from the same crystal. Don't adjust `TXFrequency` or `RXFrequency` instead.

| Hotspot modem | `FrequencyCorr` units | If it measures 300 Hz low |
|---|---|---|
| CC1200 | about 20 Hz per unit (−200 to 200) | `FrequencyCorr = 15` |
| SX1255 | Hz | `FrequencyCorr = 300` |
| MMDVM | Hz | `FrequencyCorr = 300` |

A positive value raises the frequency. Measure again after restarting. Within about ±100 Hz of nominal is good.

### Checking your radio

The same SDR can measure a radio: transmit analog FM with no audio, and read the carrier's peak. A radio's receive error matches its transmit error, since both come from one crystal. But not every radio can be corrected, and a radio that decodes poorly isn't necessarily off frequency. One OpenRTX radio tested within 50 Hz of true still decoded only about 80% of packets that another receiver decoded every time.

## Troubleshooting

**The gateway doesn't link to M17-QTC.** Check that `OverrideHosts.txt` has `M17-QTC 127.0.0.1 17000` and the gateway config says `M17-QTC`, module `A`. Look for `client linking` in `journalctl -u qtcd`. If it's missing, the gateway isn't reaching qtcd; `journalctl -u m17-gateway` shows why.

**The gateway links but voice doesn't reach the reflector.** Look for `upstream linked`. If it never appears, or qtcd logs `upstream silent; relinking`, check that `[Module A] Reflector` is spelled as it appears in `M17Hosts.txt`, and that the reflector is up. `reflector not found in hosts file` means the name isn't in the list; until it is, qtcd refuses links to that module, and it checks again every 15 minutes.

**qtcd won't start.** Run `journalctl -u qtcd -n 20`. A config error names the setting. `address already in use` on port 17000 means something else on the Pi is listening there. `bind: address already in use` on 8017 means another program is using the admin port; change `[General] Admin`.

**No `connected to bootstrap peer`.** qtcd can't reach `ham.n1adj.net` on TCP port 4001. Check the Pi's internet connection and any outgoing firewall. qtcd keeps retrying on its own.

**Some text messages arrive and some don't.** The most common cause is a hotspot that's off frequency; see [Getting your hotspot on frequency](#getting-your-hotspot-on-frequency). Radios differ too: some decode a marginal signal better than others.

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
```

With `[Inet]` and no `[Module X]`, module A is messaging-only: clients link to it for messages and rooms, and there's no voice. To pass voice to a reflector too, add a `[Module A]` as on a hotspot. qtcd then resolves the reflector's name from the M17 Project's list, downloaded daily, unless you set `HostsFile`.

Forward TCP 4001 (libp2p) and, for internet-only clients, UDP 17000 to the station. `AllowCallsigns` keeps strangers off an open client face, but it doesn't verify anyone's identity. Gateways on the address ranges in `[Inet] Gateways` (default: private ranges) aren't limited. Other nodes use the station by adding its address to `[Network] Bootstrap`: `/dns4/<name>/tcp/4001/p2p/<peer ID>`. `qtcd -config /etc/qtcd.ini -print-id` prints the peer ID.

## Known limitations

- **Messages are not private.** Nodes authenticate each other, but message content is plain text to every node that stores or forwards it, and amateur radio rules don't allow encryption anyway.
- **Callsigns are not verified.** A radio or client says who it is, and QTC believes it. Anyone could send a message that claims to be from you, or connect claiming your callsign. `AllowCallsigns` on a public station limits who can try, not who they are.
- **Nodes are trusted.** A misbehaving node could overwrite the record of which stations hold a callsign's mailbox. There are no quotas yet either. This is why the test is invite-only.
- **The protocol is a draft.** During the test, run the latest release. Releases may not work with older ones.
- **Radios have to transmit to be reachable.** qtcd sends messages to a radio only if it was heard within the last hour, and holds them otherwise. When the radio is next heard, it gets at most the 10 most recent, plus a note saying how many older ones were left out. See the [user guide](user-guide.md).
