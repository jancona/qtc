#!/bin/bash
#
# test-deb.sh checks the qtcd package's maintainer scripts: install without
# and with a gateway, config generation, the key, running as the service
# user, upgrade, remove, purge, and the rc1 JSON config upgrade. Run it
# from the repository root on a machine with Docker:
#
#     cmd/qtcd/packaging/scripts/test-deb.sh
#
# It builds two package versions for Docker's architecture into build/,
# then runs its checks in a debian:bookworm container. There is no running
# systemd in a container, so postinst's systemctl steps are skipped; test
# those on a real Pi.

set -u
V1=0.0.1~test1
V2=0.0.1~test2

if [ "${1:-}" != "--in-container" ]; then
    set -e
    case "$(docker info --format '{{.Architecture}}')" in
        aarch64|arm64) ARCH=arm64 ;;
        x86_64|amd64) ARCH=amd64 ;;
        *) echo "unsupported Docker architecture" >&2; exit 2 ;;
    esac
    for v in "$V1" "$V2"; do
        cmd/qtcd/packaging/scripts/build-deb.sh "$v" "$ARCH" >/dev/null
    done
    exec docker run --rm -e ARCH="$ARCH" -v "$PWD:/repo:ro" debian:bookworm bash -c \
        'apt-get update -qq >/dev/null && apt-get install -y -qq curl systemd >/dev/null 2>&1 &&
         bash /repo/cmd/qtcd/packaging/scripts/test-deb.sh --in-container'
fi

fail=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }

cd /tmp
n=0
for v in "$V1" "$V2"; do
    n=$((n + 1))
    cp -r "/repo/build/qtcd_${v}_${ARCH}" "pkg-$n"
    dpkg-deb --root-owner-group --build "pkg-$n" "qtcd-$n.deb" >/dev/null
done
echo "=== package info"; dpkg-deb --info qtcd-1.deb | sed -n '/Package/,$p'

echo; echo "=== 1. fresh install, no gateway, no callsign"
dpkg -i qtcd-1.deb </dev/null
check "config written with placeholders" 'grep -q CALLSIGN_PLACEHOLDER /etc/qtcd.ini'
check "config root:qtcd 640" '[ "$(stat -c %U:%G:%a /etc/qtcd.ini)" = root:qtcd:640 ]'
check "state dir qtcd 700" '[ "$(stat -c %U:%a /var/lib/qtcd)" = qtcd:700 ]'
check "no key yet" '[ ! -e /var/lib/qtcd/node.key ]'
dpkg --purge qtcd >/dev/null
check "purge removes user" '! getent passwd qtcd >/dev/null'

echo; echo "=== 2. install on a hotspot: callsign and upstream from gateway ini"
mkdir -p /opt/m17/rpi-dashboard/files
echo "M17-M17 107.191.121.105 17000" > /opt/m17/rpi-dashboard/files/M17Hosts.txt
printf '[General]\nCallsign = n1adj g\n\n[Reflector]\nName = M17-M17\nModule = t\n' > /etc/m17-gateway.ini
dpkg -i qtcd-1.deb </dev/null
cat /etc/qtcd.ini
check "node callsign N1ADJ   Q" 'grep -qx "Callsign=N1ADJ   Q" /etc/qtcd.ini'
check "upstream M17-M17 T" 'grep -qx "Reflector=M17-M17" /etc/qtcd.ini && grep -qx "Module=T" /etc/qtcd.ini'
check "hosts file from dashboard" 'grep -q /opt/m17/rpi-dashboard/files/M17Hosts.txt /etc/qtcd.ini'
check "key created, owned by qtcd" '[ "$(stat -c %U:%a /var/lib/qtcd/node.key)" = qtcd:600 ]'
ID1=$(runuser -u qtcd -- qtcd -config /etc/qtcd.ini -print-id 2>/dev/null)
echo "peer ID $ID1"

echo; echo "=== 3. qtcd runs as the service user with the generated config"
runuser -u qtcd -- qtcd -config /etc/qtcd.ini > /tmp/run.log 2>&1 &
PID=$!
sleep 4
check "admin status answers" 'curl -sf localhost:8017/status >/dev/null'
check "binary carries the package version" '[ "$(qtcd -version)" = "$V1" ]'
check "client face on 127.0.0.1:17000" 'grep -q "client face listening.*127.0.0.1:17000" /tmp/run.log'
kill -TERM $PID; wait $PID 2>/dev/null
check "delivered journal created" '[ -f /var/lib/qtcd/delivered.jsonl ]'
check "no mailbox journal without the mailbox cap" '[ ! -e /var/lib/qtcd/mailbox.jsonl ]'

echo; echo "=== 4. upgrade keeps config and key"
cp /etc/qtcd.ini /tmp/before.json
echo '# local edit' >/dev/null
sed -i 's/^MetricsInterval=15m/MetricsInterval=5m/' /etc/qtcd.ini
dpkg -i qtcd-2.deb </dev/null >/dev/null
check "version upgraded" '[ "$(dpkg-query -W -f="\${Version}" qtcd)" = "$V2" ]'
check "local config edit kept" 'grep -qx "MetricsInterval=5m" /etc/qtcd.ini'
check "same peer ID" '[ "$(runuser -u qtcd -- qtcd -config /etc/qtcd.ini -print-id 2>/dev/null)" = "$ID1" ]'

echo; echo "=== 5. remove keeps state; purge deletes it"
dpkg -r qtcd >/dev/null
check "binary gone" '[ ! -e /usr/bin/qtcd ]'
check "config kept" '[ -f /etc/qtcd.ini ]'
check "key kept" '[ -f /var/lib/qtcd/node.key ]'
dpkg --purge qtcd >/dev/null
check "config purged" '[ ! -e /etc/qtcd.ini ]'
check "state purged" '[ ! -e /var/lib/qtcd ]'
check "user purged" '! getent passwd qtcd >/dev/null'

echo; echo "=== 6. gateway already on M17-QTC; callsign from installer env"
printf '[General]\nCallsign=CALLSIGN_PLACEHOLDER\n[Reflector]\nName=M17-QTC\nModule=A\n' > /etc/m17-gateway.ini
QTCD_CALLSIGN="kc1abc-7" dpkg -i qtcd-1.deb </dev/null >/dev/null
check "callsign from env, base only" 'grep -qx "Callsign=KC1ABC  Q" /etc/qtcd.ini'
check "no self-proxy: upstream M17-M17 C" 'grep -qx "Reflector=M17-M17" /etc/qtcd.ini && grep -qx "Module=C" /etc/qtcd.ini'
dpkg --purge qtcd >/dev/null

echo; echo "=== 7. junk callsign is refused"
QTCD_CALLSIGN='x; rm -rf /' dpkg -i qtcd-1.deb </dev/null >/dev/null 2>&1
check "junk left as placeholder" 'grep -q CALLSIGN_PLACEHOLDER /etc/qtcd.ini'
check "status still installed" '[ "$(dpkg-query -W -f="\${Status}" qtcd)" = "install ok installed" ]'

echo; echo "=== 8. rc1 JSON config is set aside, key kept"
dpkg --purge qtcd >/dev/null 2>&1
printf '[General]\nCallsign=N1ADJ G\n[Reflector]\nName=M17-M17\nModule=T\n' > /etc/m17-gateway.ini
dpkg -i qtcd-1.deb </dev/null >/dev/null
ID8=$(runuser -u qtcd -- qtcd -config /etc/qtcd.ini -print-id 2>/dev/null)
mv /etc/qtcd.ini /tmp/keep.ini
echo '{"callsign": "N1ADJ   Q"}' > /etc/qtcd.json
dpkg -i qtcd-2.deb </dev/null | grep -i "INI"
check "old json set aside" '[ -f /etc/qtcd.json.rc1 ] && [ ! -e /etc/qtcd.json ]'
check "new ini written" 'grep -qx "Callsign=N1ADJ   Q" /etc/qtcd.ini'
check "key kept across format change" '[ "$(runuser -u qtcd -- qtcd -config /etc/qtcd.ini -print-id 2>/dev/null)" = "$ID8" ]'
dpkg --purge qtcd >/dev/null
check "purge removes rc1 leftovers" '[ ! -e /etc/qtcd.json.rc1 ] && [ ! -e /etc/qtcd.ini ]'

echo; [ $fail = 0 ] && echo "ALL PASS" || echo "SOME FAILED"
[ $fail = 0 ]
