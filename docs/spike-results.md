# Spike results

**Status:** complete for the single-network case, 2026-09-15. Milestone 1 of the architecture (§9).

## Setup

| Role | Machine | Node callsign | Notes |
|---|---|---|---|
| Public station: `public`, `relay`, `mailbox` | Raspberry Pi 5, 4 GB, Raspbian 12, 44net address 44.27.19.158 (`ham.n1adj.net`) | `N1ADJ  P` | Listens on TCP 4001. DHT server. |
| Station A | Raspberry Pi Zero 2 W, 415 MB, Debian 13, running `m17-gateway` with an SX1255 modem | `N1ADJ  Z` | Homes `N1ADJ  H`. The memory and CPU target. |
| Station B | MacBook Pro | `N1ADJ  M` | Homes `N0CALL` (placeholder, nothing on the air) so its mailbox differs from A's. |

station commit: see git log for the deployment day. Config: presence every 5 min, sweep hourly, metrics every 60 s, admin HTTP on loopback for injecting messages by hand (stands in for the M17_inet face, milestone 2). Mailbox membership is static (`mailbox_members`); DHT mailbox records are milestone 5.

All three machines were on one home LAN. The first run bootstrapped over LAN addresses; the second bootstrapped over the public station's 44net address (`/dns4/ham.n1adj.net/tcp/4001`) after TCP 4001 was opened, so the two stations reached the public station over the internet even though they sit on the same LAN.

## What worked

- **Unicast with receipts.** `N1ADJ  H` on A to `N1ADJ  D` on B, RCPT_REQ set: delivered on B about 10 ms after the mailbox node's EVENT, and A received both QUEUED (from itself) and TRANSMITTED (from B, with B's last-heard time for the device). Mailbox node held 3 envelopes afterwards: the message and two receipts.
- **Subscription state follows the callsign.** A JOIN on B for `N1ADJ  D` was stored on N1ADJ's mailbox and A applied it from the EVENT, joining the room topic itself.
- **Room over gossipsub.** After changing B's device to `N0CALL`, a room message from A reached B over the `/qtc/0/room/MAINE` topic. A and B connected to each other after learning of one another from presence.
- **Presence.** All three nodes see each other's cards and heard devices within one publish interval.
- **Internet path.** With bootstrap on the 44net address, both stations connected to the public station at 44.27.19.158:4001 (the public station saw them from the home network's public address). Unicast with receipts and the room message both worked over that path.
- **Circuit relay.** Stations without the `public` capability declare themselves private, reserve a circuit relay slot on their bootstrap peers, and advertise `/p2p-circuit` addresses. The two stations connected to each other through the public station's circuit relay (`.../tcp/4001/p2p/<public>/p2p-circuit`) about 25 s after start, and the room message flowed over that relayed gossipsub link. Hole punching did not upgrade to a direct connection, which is expected with both behind the same NAT; a second home network is needed to test that.

## Bugs found by the run, all fixed

- Stop deadlocked waiting on store event loops before closing their streams.
- A node with no local callsigns never published presence, so its card was undiscoverable. Spec changed to publish regardless (node protocol §3).
- Homing gave up its first WATCH and sweep until the hourly sweep if the mailbox node was unreachable at start. Now retried every 30 s until the first success.
- Stations never connected to each other, so room topics had no path. Now each station dials the peers it learns from presence.
- The first version of that dialer tried once and then waited 5 minutes; a DHT lookup that ran before the routing table had filled failed with "not found" and the stations stayed apart. It now retries with backoff from 30 s to 5 min, and falls back to a relayed address through each bootstrap peer, which a NATed station can always use.
- A node that restarted missed the presence others had already published and was not learned for up to 5 minutes. Nodes now republish presence when a new node appears.
- The public station's circuit relay service ran with libp2p's default limits (2 min, 128 KB per relayed connection), which would cut a relayed gossipsub link; the spike lifts the limits. Whether production nodes should is an open question (Node Protocol §12).
- Found by the soak: when a store stream to a mailbox member dropped (the Mac slept), nothing reopened it, so the WATCH was gone until the next hourly sweep and a message sent five hours in was not delivered. The event loop now reopens the stream with backoff, which re-sends the WATCH, and sweeps each homed callsign. Verified by restarting the mailbox node: both stations reopened within 15 s and the next message was delivered with receipts.

## Measurements

Binary (`cmd/qtcd`, linux/arm64, `-trimpath -ldflags "-s -w"`): 26.1 MB.

Pi Zero 2 W baseline before station, `m17-gateway` alone: load average 2.5, about 56 % CPU busy over 4 cores, 100 MB free, gateway RSS 11 MB at 199 % CPU (SX1255 DSP).

Station on the Pi Zero after start-up and the tests above, idle:

| Metric | Value |
|---|---|
| RSS | 27 to 28 MB |
| Go heap | 1 to 2 MB |
| Goroutines | 78 to 84 |
| Threads | 9 to 10 |
| CPU | 0.3 to 0.5 % of one core |
| Load average with station | 2.4 to 2.7 (unchanged from baseline) |
| Free memory | 83 MB (100 MB before station) |
| Peers | 2 |

Public station on the Pi 5 with circuit relay service and mailbox: RSS 27 MB, 80 goroutines, 0 % CPU. Mac: RSS 33 MB.

Five-hour soak on the Pi Zero (16:24 to 21:24, 299 one-minute samples), station idle apart from presence and the Mac's sleep/wake reconnections:

| Metric | Range over the soak |
|---|---|
| RSS at the end | 28 MB (max RSS 27 MB reported by the process itself, flat from the first sample) |
| Go heap | 1 to 2 MB |
| Goroutines | 74 to 85 |
| CPU | 0 % in every sample; 0.1 % of one core as the process average |
| Warnings or errors logged | 0 |
| Load average at the end | 2.75 / 2.48 / 2.45 against a 2.5 baseline |
| Free memory | 84 MB, against 100 MB before station |
| Log growth | 97 KB over five hours at debug level |

`m17-gateway` was unaffected: 11 MB RSS and two cores of SX1255 DSP before and after.

The public station over the same period: RSS 28 MB, 66 goroutines, 0 % CPU, and it expired the test messages on schedule (stored count fell from 4 to 2 as the 60-minute TTLs ran out). The Mac slept and woke at least three times; each time it reconnected to the relay and re-established the relayed connection to the hotspot within about 10 s.

## Open items from the run

- Hole punching untested: both stations were behind the same NAT. Needs a station on a second home network.
- Station-to-station connection relies on presence-driven dialing plus the circuit relay. The alternative is public stations relaying room topics; decide when the second home network is available.
- The delivered-once table is in memory; a restart replays messages within the replay window that have no DELIVERED receipt (seen once during the run). Native clients dedup by ID; it is the spec's stated behaviour for legacy radios.
- `N0CALL` placeholder device on B; replace with a real second callsign for any on-air test.

## Decision: does libp2p earn its weight?

Yes, on this evidence. What it cost on the Pi Zero 2 W: 28 MB of RSS, no measurable CPU, about 16 MB of free memory, and a 26 MB binary, alongside a gateway already running a software modem on two cores. What it gave for free: TLS-authenticated transport, peer identity from the node key, NAT traversal through a circuit relay with hole punching available, gossipsub for presence and rooms, and a DHT ready for mailbox records. The bugs found during the run were all in station's use of those pieces (reconnection, dialing, presence timing), not in the pieces themselves. The remaining risk is the untested hole-punching path between two different NATs, which does not change the decision because the circuit relay path works and its cost is the public station's bandwidth.

## Milestone 2: proxy reflector, first run (2026-09-21)

Setup: an m17-gateway hotspot (`cc1200trixie`, CC1200 modem, callsign `N1ADJ   C`) on a travel LAN behind Starlink, linked to `M17-QTC` module A served by a qtcd station on a laptop on the same LAN. Module A maps to `M17-M17` module T in qtc mode. The laptop station reached the public station on the Pi 5 over the internet through a WireGuard tunnel. The gateway's only change was one line in its override hosts file and the reflector name and module in its configuration.

- The gateway linked and received M17-M17's ACKN through the proxy in about 80 ms, and stayed linked; PING and PONG pass both ways.
- A voice transmission from an OpenRTX CS7000 reached M17-M17 module T, and M17-M17's traffic came back to the gateway through the proxy (the gateway logged the returning stream). Voice is unaffected, which is the milestone's success criterion.
- The station published presence for the radio's callsign (`N1ADJ 8`, via RF) within the first presence interval, and the public station's table showed it. The station homed N1ADJ and auto-subscribed it to the local room.

Not yet tested: SMS in either direction, since the radio's firmware at hand has no SMS support. The room command and message paths are covered by the in-process client face test and wait for a radio that can send SMS.

## Milestones 2 and 3: SMS on the air (2026-09-22)

Same hotspot and laptop station as the first run, radio now an OpenRTX CS7000 with SMS. The public station ran on the laptop too (the Pi 5 was reachable but not needed), hearing a test device `N0CALL` through its configuration.

Every planned test passed on the air:

- **Room commands.** `/rooms` and `/join TEST` sent as SMS to the node callsign were answered by SMS from the node.
- **Room message.** `#TEST net tonight at 7` to the node callsign was stored as a message to room TEST with the marker stripped, and echoed back to the radio (echo enabled for the test) within 5 ms of receipt.
- **Unicast out.** SMS to `N0CALL` was delivered at the public station's device.
- **Unicast in.** A MSG from `N0CALL` with a receipt request reached the radio as SMS within a second; QUEUED and TRANSMITTED receipts came back to N0CALL, the latter carrying the radio's last-heard time.
- **Bad room name.** `#MA.INE hello` was answered with an error SMS. The first attempt's reply, with quoted names and an apostrophe, was transmitted but not seen on the radio; a plain `error: bad room name MA.INE` was.
- **Undeliverable.** SMS to `W1AW` landed in W1AW's mailbox and nothing was transmitted.
- **Native mode.** On module B, three packets and a voice transmission passed to M17-M17 with no mailbox change and no presence published.

Bugs and conventions that came out of the run, all fixed and in the specs:

- The radio sent the node callsign as `N1ADJ M`, one space; the node now matches its callsign ignoring runs of spaces (node protocol §10).
- Legacy radios cannot enter Extended addresses, so `#NAME text` to the node callsign addresses a room, and room messages are delivered as SMS to the device with `#NAME ` prefixed. Commands accept an optional `#`. Echo to the sender's device is a configuration option, off by default (rooms §3.1, §6).
- The echo option was not wired into the daemon's config parsing at first.
- A station that is itself a mailbox never saw its own stores; the store now reports them through a hook and local puts and sweeps include the station's own store.
- Relinking through the proxy was fragile: the station did not DISC upstream on shutdown, so the reflector kept a stale link and NACKed the next CONN, and the gateway treated NACK as final. The node now answers the client's CONN itself, resends an unanswered upstream CONN, and after one case where M17-M17 acknowledged but never pinged, owns both keepalives: it PINGs its client and answers the reflector's PINGs, relinking upstream after 30 s of silence (node protocol §10).

## Hole punching across two NATs (2026-09-22)

The one path the spike had not exercised. Setup: the public station on the Pi 5 (44.27.19.158, home network, 44net address); a station on an Intel box on the home LAN behind the home router (public address 71.181.76.200); a station on the laptop behind Starlink's CGNAT (public address 153.66.125.182), with WireGuard disconnected so nothing could shortcut through the home tunnel, and the laptop's libp2p host bound to the Starlink interface only.

- Both stations linked to the public station over the internet and reserved circuit relay slots.
- Within 60 s of the laptop starting, the two stations had learned of each other from presence, connected through the public station's circuit relay, and hole-punched a direct TCP connection: the laptop saw the home station at 71.181.76.200:34279 and the home station saw the laptop at 153.66.125.182:43908. The home station then dropped its relayed path and kept only the direct one; the laptop kept both.
- A message from the laptop's device to the home station's device was delivered and both receipts came back within a second.

So hole punching works through Starlink's CGNAT to a home router, at least for this pair, and the relay fallback was there for the seconds before it did. That closes the last open item from the spike's decision on libp2p.

## Milestone 5: mailbox records (2026-09-22, in-process only)

Records replace the static member list. An in-process test with three public mailbox stations and two stations behind them checks: the first station to home a callsign writes version 1 naming itself home station and two mailbox-capable public stations; a sender reads it from the DHT and delivers through those members; a sender's message to an unheard callsign creates a provisional record that the first station to hear the callsign takes over as version 2, after which the stored message reaches the device by sweep; and when a member is stopped, the home station's sweeps count the failures, the member's presence silence passes the configured period, and the home station writes version 2 replacing it with the remaining public station after copying the callsign's full history there. Not yet run on real hardware.

## Milestone 5 on real nodes (2026-09-26)

Home LAN: public mailbox stations on the Pi 5 (`N1ADJ  P`), the Intel box (`N1ADJ  X`), and a second process on the laptop (`N1ADJ  Y`); the home station `N1ADJ  M` on the laptop with the client face, the CC1200 hotspot linked to it, and the CS7000 radio as `N1ADJ 8`. The home station ran with sweeps every minute, member failure after 3 unreachable sweeps and 2 minutes of presence silence, and a 3-minute takeover period.

| Step | Result |
|---|---|
| Creation | Keying up produced version 1 within the same second: home station the laptop, members the Intel box and the Pi 5. |
| Sender lookup | The Pi 5 had learned the record from the announcement; a message from its `N0CALL` device reached the radio as SMS in under a second, with QUEUED and TRANSMITTED receipts back. |
| Provisional handoff | A message from `N0CALL` to the never-heard `AB1CD` produced a provisional version 1 naming the Pi 5; when the Intel box heard `AB1CD` it wrote version 2 within a second and the stored message was delivered there. |
| Repair | Stopping the Intel box: three failed sweeps at one-minute intervals, then version 2 of N1ADJ's record replaced it with the laptop's public station, which received a copy of the history. Two and a half minutes from stop to repair. |
| Takeover | Stopping the home station and having the laptop's public station hear the radio produced a takeover, but instantly and for the wrong reason (below). After the fix, a proper run: the home station saw the new home station silent for exactly 3 minutes and wrote version 4, then repaired the stopped member out as version 5. |
| Top-up | With only one member left and the others back, the next sweep wrote version 6 with two members. |
| Messaging on the final record | SMS from the radio to `N0CALL` delivered at the Pi 5; reply from `N0CALL` delivered to the radio with receipts, both through version 6. |

Two things the run found that the in-process test had not:

- A freshly started station treated a node it had never seen in presence as silent forever, and took over a record on that basis within a second of hearing the callsign. Silence of an unknown node is now counted from the station's own start (node protocol §8.4).
- Repair only fired on failures, so a record left below `k` when no replacement was available stayed short. The home station now recruits on a later sweep once a candidate is known (§8.2).

Also noted: a station's presence table can lag the network by up to one presence interval after a restart, which made the first repair pick from fewer candidates than existed. Harmless here; worth remembering when reading logs.
