# Test bed

How the hardware runs in `spike-results.md` were set up, so the next session or machine can repeat them. Nothing here is protocol; it is operations.

## Machines

| Name | Address | SSH | Arch | Roles used |
|---|---|---|---|---|
| Pi 5 `ham` | 192.168.1.173 on the home LAN; 44.27.19.158 (`ham.n1adj.net`) on 44net; `openwebrx.home.anconafamily.com` | `pi@` | arm64 | public station: `public`, `relay`, `mailbox`, DHT server. TCP 4001 is open to the internet on the 44net address. |
| Intel box `chromebox` | 192.168.1.140; `chromebox.home.anconafamily.com` | `jim@` | amd64 | public mailbox station on the home LAN; a station behind the home NAT for the hole-punching run |
| Hotspot `cc1200trixie` | `cc1200trixie.local` (192.168.1.135 at home) | `jim@`, passwordless sudo | arm64 | runs `m17-gateway` with a CC1200 modem; the legacy client |
| Hotspot `sx1255test` | `sx1255test.local` | `jim@` | arm64, Pi Zero 2 W | the spike's memory target; runs `m17-gateway` with an SX1255 modem |
| Laptop | 192.168.1.105 at home | local | darwin | home station with the client face; extra public mailbox stations as separate processes |
| Radio | CS7000 running OpenRTX with an SMS client | | | appears as `N1ADJ 8` |

Node callsigns used: `N1ADJ  P` Pi 5, `N1ADJ  X` Intel box, `N1ADJ  Y` laptop public station, `N1ADJ  M` laptop home station, `N1ADJ  Z` Pi Zero. Test devices injected by config or admin: `N0CALL`, `AB1CD`, `N1ADJ  H`.

Away from home, WireGuard reaches the home LAN. The 44Connect router at 192.168.1.44 has no route back to the WireGuard subnet, so it is reachable only through an SSH port forward via the Pi 5. Starlink is CGNAT; hole punching through it worked.

## Building

```
cd qtc
go build -o /tmp/qtcd-mac ./cmd/qtcd
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /tmp/qtcd-linux-arm64 ./cmd/qtcd
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /tmp/qtcd-linux-amd64 ./cmd/qtcd
go build -o /tmp/qtc ./cmd/qtc
```

Each machine keeps `~/qtcd/` with `qtcd`, `qtcd.json`, `node.key`, and `qtcd.log`. Keep the key file so the peer ID stays the same; the Pi 5's is `QmdxViFZP4PK5TL3xANxScyaFnvBfdqfbQqjjJGhvJvuSg`. Copying a binary over a running one fails with "text file busy": stop first.

## Configs

Public station (Pi 5):

```json
{"callsign": "N1ADJ  P", "data_dir": "/home/pi/qtcd",
 "listen": ["/ip4/0.0.0.0/tcp/4001"], "bootstrap": [],
 "caps": ["public", "relay", "mailbox"], "software": "qtcd/0.1", "dht": true,
 "metrics_interval": "60s", "admin": "127.0.0.1:8017"}
```

A second public mailbox station bootstraps to the first: `"bootstrap": ["/ip4/192.168.1.173/tcp/4001/p2p/<Pi 5 peer ID>"]`. On the laptop, bind `listen` to the LAN address with a distinct port and a distinct `admin` port.

Home station with the client face (laptop), shortened periods for lifecycle tests:

```json
{"callsign": "N1ADJ  M", "data_dir": "<dir>", "listen": ["/ip4/192.168.1.105/tcp/0"],
 "bootstrap": ["/ip4/192.168.1.173/tcp/4001/p2p/<Pi 5 peer ID>"], "caps": [], "dht": true, "k": 2,
 "presence_interval": "60s", "sweep_interval": "60s", "record_refresh": "30s",
 "member_fail_sweeps": 3, "member_fail_silence": "2m", "takeover_period": "3m",
 "echo_room_messages": true, "metrics_interval": "60s", "admin": "127.0.0.1:8018",
 "inet": {"listen": "0.0.0.0:17000", "hosts_file": "<dir>/M17Hosts.txt",
   "modules": {"A": {"reflector": "M17-M17", "module": "T", "mode": "qtc"},
               "B": {"reflector": "M17-M17", "module": "T", "mode": "native"}}}}
```

`M17Hosts.txt` comes from the hotspot: `/opt/m17/rpi-dashboard/files/M17Hosts.txt`. Away from home, bootstrap with `/dns4/ham.n1adj.net/tcp/4001/p2p/<ID>`. `mailbox_members` is only a seed list now; leave it out when presence will show mailbox stations. Production values: presence 5 min, sweep 1 h, failure 24 h and 3 sweeps, takeover 7 days, echo off.

## Bring-up

Order: public stations first, then home stations, then the gateway.

```
ssh pi@192.168.1.173 'cd ~/qtcd && pkill -x qtcd; sleep 1; (nohup ./qtcd -config qtcd.json -log-level debug > qtcd.log 2>&1 &)'
```

Use `pkill -x qtcd`, never `pkill -f "qtcd -config"`: the pattern matches the SSH session's own command line and kills it. Redirect stdin from `/dev/null` on SSH commands that start background processes, or the session may not return.

Pointing the hotspot's gateway at the home station, where `<laptop>` is the laptop's LAN address (revert by restoring the backup and removing the override line):

```
sudo cp -n /etc/m17-gateway.ini /etc/m17-gateway.ini.pre-qtc
echo "M17-QTC <laptop> 17000" | sudo tee -a /opt/m17/rpi-dashboard/files/OverrideHosts.txt
sudo sed -i -E "s/^Name = .*/Name = M17-QTC/; s/^Module = .*/Module = A/" /etc/m17-gateway.ini
sudo systemctl restart m17-gateway
```

The gateway relinks by itself after a home station restart, with backoff up to a few minutes; `systemctl restart m17-gateway` is faster.

## Driving and observing

- Admin status: `curl -s localhost:8018/status` shows peers with the address each connection uses (`/p2p-circuit` means relayed), presence, homed callsigns, rooms, mailbox records, and stored count on mailbox stations.
- Inject a device or a message: `curl -X POST 'localhost:8017/heard?device=N0CALL'`, `qtc send -admin 127.0.0.1:8017 -from N0CALL -to "N1ADJ 8" -body hello -rcpt`, `curl -X POST 'localhost:8018/room?device=N0CALL&op=join&rooms=TEST'`.
- Read a mailbox: `qtc store query -peer /ip4/192.168.1.173/tcp/4001/p2p/<ID> -callsign W1AW`.
- Logs: `grep -E "deliver|record written|client linking|upstream linked|WARN|ERROR" qtcd.log`. Control datagrams (PING, PONG, ACKN) are logged at debug as `control from client` and `control from upstream`.
- Gateway: `sudo journalctl -u m17-gateway --since "-5min" | grep -E "Received ACKN|No PINGs|packet dst|TransmitPacket"`.

From the radio: SMS to `N1ADJ M` (one space works) with `/rooms`, `/join TEST`, `#TEST text`; SMS to a callsign for unicast. Legacy radios get no receipts.

## Teardown

Restore the gateway (`sudo cp /etc/m17-gateway.ini.pre-qtc /etc/m17-gateway.ini`, delete the `M17-QTC` line, restart), then `pkill -x qtcd` on the test stations. The Pi 5's public station can stay up.

## Things that bit us

- Radio UIs send `N1ADJ M`, one space; the node matches its callsign ignoring space runs.
- Stopping a station without DISC left M17-M17 holding the old link and refusing the next CONN; fixed in code, but a killed station still leaves a stale link for the reflector's timeout.
- A freshly started station has an empty presence table for up to one presence interval; records created in that window see fewer candidates.
- With `data_dir` set, mailboxes and the delivered-once table are journaled there (`mailbox.jsonl`, `delivered.jsonl`) and survive a restart. Without it they are in memory: a restart empties them and the replay window can redeliver. `data_dir` also defaults the key to `<data_dir>/node.key`, so pointing it at `~/qtcd` keeps the existing peer ID.
- The scratch directory holding laptop keys and configs is per session; keep test configs somewhere durable.
