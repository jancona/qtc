# Test bed

How the hardware runs in `spike-results.md` were set up, so they can be repeated. Nothing here is protocol; it is operations. Addresses are placeholders: `<pi5-lan>`, `<laptop-lan>` and so on stand for your own machines' LAN addresses.

## Machines

| Role | Hardware | Arch | Roles used |
|---|---|---|---|
| Public station | Raspberry Pi 5 with a public address (`ham.n1adj.net` for the test network) | arm64 | `public`, `relay`, `mailbox`, DHT server. TCP 4001 open to the internet. |
| Second public mailbox station | any Linux box on the LAN | amd64 | public mailbox station; also a station behind NAT for hole-punching runs |
| Hotspot | Pi with a CC1200 modem running `m17-gateway` | arm64 | the legacy client |
| Low-memory hotspot | Pi Zero 2 W with an SX1255 modem running `m17-gateway` | arm64 | the memory target |
| Laptop | | darwin | home station with the client face; extra public mailbox stations as separate processes |
| Radio | CS7000 running OpenRTX with an SMS client | | appears as `N1ADJ 8` |

Node callsigns used: `N1ADJ  P` Pi 5, `N1ADJ  X` second public station, `N1ADJ  Y` laptop public station, `N1ADJ  M` laptop home station, `N1ADJ  Z` Pi Zero. Test devices injected by config or admin: `N0CALL`, `AB1CD`, `N1ADJ  H`.

Hole punching has been tested through Starlink's CGNAT to a home router.

## Building

For a release or a hotspot, use the package (`cmd/qtcd/packaging/scripts/build-deb.sh VERSION ARCH`, or a CI release). For quick test binaries:

```
cd qtc
go build -o /tmp/qtcd-mac ./cmd/qtcd
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /tmp/qtcd-linux-arm64 ./cmd/qtcd
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /tmp/qtcd-linux-amd64 ./cmd/qtcd
go build -o /tmp/qtc ./cmd/qtc
```

A machine run by hand keeps `~/qtcd/` with `qtcd`, `qtcd.json`, the state files, and `qtcd.log`. Keep `node.key` so the peer ID stays the same; the test network's public station is `QmdxViFZP4PK5TL3xANxScyaFnvBfdqfbQqjjJGhvJvuSg`. Copying a binary over a running one fails with "text file busy": stop first.

## Configs

Public station:

```json
{"callsign": "N1ADJ  P", "data_dir": "/home/pi/qtcd",
 "listen": ["/ip4/0.0.0.0/tcp/4001"], "bootstrap": [],
 "caps": ["public", "relay", "mailbox"], "dht": true,
 "metrics_interval": "60s", "admin": "127.0.0.1:8017"}
```

A second public mailbox station bootstraps to the first: `"bootstrap": ["/ip4/<pi5-lan>/tcp/4001/p2p/<Pi 5 peer ID>"]`. On the laptop, bind `listen` to the LAN address with a distinct port and a distinct `admin` port.

Home station with the client face (laptop), shortened periods for lifecycle tests:

```json
{"callsign": "N1ADJ  M", "data_dir": "<dir>", "listen": ["/ip4/<laptop-lan>/tcp/0"],
 "bootstrap": ["/ip4/<pi5-lan>/tcp/4001/p2p/<Pi 5 peer ID>"], "caps": [], "dht": true, "k": 2,
 "presence_interval": "60s", "sweep_interval": "60s", "record_refresh": "30s",
 "member_fail_sweeps": 3, "member_fail_silence": "2m", "takeover_period": "3m",
 "echo_room_messages": true, "metrics_interval": "60s", "admin": "127.0.0.1:8018",
 "inet": {"listen": "0.0.0.0:17000", "hosts_file": "<dir>/M17Hosts.txt",
   "modules": {"A": {"reflector": "M17-M17", "module": "T", "mode": "qtc"},
               "B": {"reflector": "M17-M17", "module": "T", "mode": "native"}}}}
```

`M17Hosts.txt` comes from a hotspot: `/opt/m17/rpi-dashboard/files/M17Hosts.txt`. Off the LAN, bootstrap with `/dns4/ham.n1adj.net/tcp/4001/p2p/<ID>`. `mailbox_members` is only a seed list now; leave it out when presence will show mailbox stations. Production values: presence 5 min, sweep 1 h, failure 24 h and 3 sweeps, takeover 7 days, echo off.

## Bring-up

Order: public stations first, then home stations, then the gateway.

```
ssh <user>@<pi5-lan> 'cd ~/qtcd && pkill -x qtcd; sleep 1; (nohup ./qtcd -config qtcd.json -log-level debug > qtcd.log 2>&1 &)' < /dev/null
```

Use `pkill -x qtcd`, never `pkill -f "qtcd -config"`: the pattern matches the SSH session's own command line and kills it. Redirect stdin from `/dev/null` on SSH commands that start background processes, or the session may not return.

Pointing the hotspot's gateway at a home station on another machine, where `<laptop-lan>` is that machine's LAN address (revert by restoring the backup and removing the override line):

```
sudo cp -n /etc/m17-gateway.ini /etc/m17-gateway.ini.pre-qtc
echo "M17-QTC <laptop-lan> 17000" | sudo tee -a /opt/m17/rpi-dashboard/files/OverrideHosts.txt
sudo sed -i -E "s/^Name = .*/Name = M17-QTC/; s/^Module = .*/Module = A/" /etc/m17-gateway.ini
sudo systemctl restart m17-gateway
```

With the `qtcd` package on the hotspot itself, the override line is `M17-QTC 127.0.0.1 17000`.

The gateway relinks by itself after a home station restart, with backoff up to a few minutes; `systemctl restart m17-gateway` is faster.

## Driving and observing

- Admin status: `curl -s localhost:8018/status` shows peers with the address each connection uses (`/p2p-circuit` means relayed), presence, homed callsigns, rooms, mailbox records, and stored count on mailbox stations.
- Inject a device or a message: `curl -X POST 'localhost:8017/heard?device=N0CALL'`, `qtc send -admin 127.0.0.1:8017 -from N0CALL -to "N1ADJ 8" -body hello -rcpt`, `curl -X POST 'localhost:8018/room?device=N0CALL&op=join&rooms=TEST'`.
- Read a mailbox: `qtc store query -peer /ip4/<pi5-lan>/tcp/4001/p2p/<ID> -callsign W1AW`.
- Logs: `grep -E "deliver|record written|client linking|upstream linked|WARN|ERROR" qtcd.log`, or for the package `journalctl -u qtcd -p warning`. Control datagrams (PING, PONG, ACKN) are logged at debug as `control from client` and `control from upstream`.
- Gateway: `sudo journalctl -u m17-gateway --since "-5min" | grep -E "Received ACKN|No PINGs|packet dst|TransmitPacket"`.

From the radio: SMS to `N1ADJ M` (one space works) with `/rooms`, `/join TEST`, `#TEST text`; SMS to a callsign for unicast. Legacy radios get no receipts.

## Teardown

Restore the gateway (`sudo cp /etc/m17-gateway.ini.pre-qtc /etc/m17-gateway.ini`, delete the `M17-QTC` line, restart), then `pkill -x qtcd` on the test stations. The public station can stay up.

## Things that bit us

- Radio UIs send `N1ADJ M`, one space; the node matches its callsign ignoring space runs.
- Stopping a station without DISC left M17-M17 holding the old link and refusing the next CONN; fixed in code, but a killed station still leaves a stale link for the reflector's timeout.
- A freshly started station has an empty presence table for up to one presence interval; records created in that window see fewer candidates.
- With `data_dir` set, mailboxes and the delivered-once table are journaled there (`mailbox.jsonl`, `delivered.jsonl`) and survive a restart. Without it they are in memory: a restart empties them and the replay window can redeliver. `data_dir` also defaults the key to `<data_dir>/node.key`, so pointing it at `~/qtcd` keeps the existing peer ID.
- A temporary directory holding test keys and configs may not outlive the session; keep test configs somewhere durable.
