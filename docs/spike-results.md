# Spike results

**Status:** in progress, 2026-09-15. Milestone 1 of the architecture (§9).

## Setup

| Role | Machine | Node callsign | Notes |
|---|---|---|---|
| Public node: `public`, `relay`, `inbox` | Raspberry Pi 5, 4 GB, Raspbian 12, 44net address 44.27.19.158 (`ham.n1adj.net`) | `N1ADJ  P` | Listens on TCP 4001. DHT server. |
| Roost A | Raspberry Pi Zero 2 W, 415 MB, Debian 13, running `m17-gateway` with an SX1255 modem | `N1ADJ  Z` | Homes `N1ADJ  H`. The memory and CPU target. |
| Roost B | MacBook Pro | `N1ADJ  M` | Homes `N0CALL` (placeholder, nothing on the air) so its inbox differs from A's. |

roost commit: see git log for the deployment day. Config: presence every 5 min, sweep hourly, metrics every 60 s, admin HTTP on loopback for injecting messages by hand (stands in for the M17_inet face, milestone 2). Inbox membership is static (`inbox_members`); DHT inbox records are milestone 5.

All three machines were on one home LAN. The first run bootstrapped over LAN addresses; the second bootstrapped over the public node's 44net address (`/dns4/ham.n1adj.net/tcp/4001`) after TCP 4001 was opened, so the two roosts reached the public node over the internet even though they sit on the same LAN.

## What worked

- **Unicast with receipts.** `N1ADJ  H` on A to `N1ADJ  D` on B, RCPT_REQ set: delivered on B about 10 ms after the inbox node's EVENT, and A received both QUEUED (from itself) and TRANSMITTED (from B, with B's last-heard time for the device). Inbox node held 3 envelopes afterwards: the message and two receipts.
- **Subscription state follows the callsign.** A JOIN on B for `N1ADJ  D` was stored on N1ADJ's inbox and A applied it from the EVENT, joining the room topic itself.
- **Room over gossipsub.** After changing B's device to `N0CALL`, a room message from A reached B over the `/pigeon/0/room/MAINE` topic. A and B connected to each other after learning of one another from presence.
- **Presence.** All three nodes see each other's cards and heard devices within one publish interval.
- **Internet path.** With bootstrap on the 44net address, both roosts connected to the public node at 44.27.19.158:4001 (the public node saw them from the home network's public address). Unicast with receipts and the room message both worked over that path.
- **Relay.** Roosts without the `public` capability declare themselves private, reserve a relay slot on their bootstrap peers, and advertise `/p2p-circuit` addresses. The two roosts connected to each other through the public node's relay (`.../tcp/4001/p2p/<public>/p2p-circuit`) about 25 s after start, and the room message flowed over that relayed gossipsub link. Hole punching did not upgrade to a direct connection, which is expected with both behind the same NAT; a second home network is needed to test that.

## Bugs found by the run, all fixed

- Stop deadlocked waiting on store event loops before closing their streams.
- A node with no local callsigns never published presence, so its card was undiscoverable. Spec changed to publish regardless (node protocol §3).
- Homing gave up its first WATCH and sweep until the hourly sweep if the inbox node was unreachable at start. Now retried every 30 s until the first success.
- Roosts never connected to each other, so room topics had no path. Now each roost dials the peers it learns from presence.
- The first version of that dialer tried once and then waited 5 minutes; a DHT lookup that ran before the routing table had filled failed with "not found" and the roosts stayed apart. It now retries with backoff from 30 s to 5 min, and falls back to a relayed address through each bootstrap peer, which a NATed roost can always use.
- A node that restarted missed the presence others had already published and was not learned for up to 5 minutes. Nodes now republish presence when a new node appears.
- The public node's relay service ran with libp2p's default limits (2 min, 128 KB per relayed connection), which would cut a relayed gossipsub link; the spike lifts the limits. Whether production nodes should is an open question (Node Protocol §12).

## Measurements

Binary (`cmd/roost`, linux/arm64, `-trimpath -ldflags "-s -w"`): 26.1 MB.

Pi Zero 2 W baseline before roost, `m17-gateway` alone: load average 2.5, about 56 % CPU busy over 4 cores, 100 MB free, gateway RSS 11 MB at 199 % CPU (SX1255 DSP).

Roost on the Pi Zero after start-up and the tests above, idle:

| Metric | Value |
|---|---|
| RSS | 27 to 28 MB |
| Go heap | 1 to 2 MB |
| Goroutines | 78 to 84 |
| Threads | 9 to 10 |
| CPU | 0.3 to 0.5 % of one core |
| Load average with roost | 2.4 to 2.7 (unchanged from baseline) |
| Free memory | 83 MB (100 MB before roost) |
| Peers | 2 |

Public node on the Pi 5 with relay service and inbox: RSS 27 MB, 80 goroutines, 0 % CPU. Mac: RSS 33 MB.

One-hour soak: pending.

## Open items from the run

- Hole punching untested: both roosts were behind the same NAT. Needs a roost on a second home network.
- Roost-to-roost connection relies on presence-driven dialing plus the relay. The alternative is public nodes relaying room topics; decide when the second home network is available.
- The delivered-once table is in memory; a restart replays messages within the replay window that have no DELIVERED receipt (seen once during the run). Native clients dedup by ID; it is the spec's stated behaviour for legacy radios.
- `N0CALL` placeholder device on B; replace with a real second callsign for any on-air test.

## Decision: does libp2p earn its weight?

To be written after the soak and the internet-path test. Initial read: 27 MB RSS and no measurable CPU on a Pi Zero 2 W already running a software modem is well within budget; the 26 MB binary is the main cost.
