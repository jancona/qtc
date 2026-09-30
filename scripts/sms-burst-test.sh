#!/bin/bash
#
# sms-burst-test.sh: which gateway setting lets a radio decode every SMS?
# For each value of one m17-gateway setting, restarts the gateway with it
# and sends a numbered burst to a radio through qtcd. Read off, on the
# radio, which messages decoded; each body names the value and the number.
#
# Run on the hotspot, as a user with sudo:
#
#   ./sms-burst-test.sh                                # PacketGap 0.25s 0.5s 1s 2s
#   COUNT=10 ./sms-burst-test.sh 0.5s 1s               # choose count and values
#   SETTING=RXHoldoff FIX="Radio.PacketGap=2s" ./sms-burst-test.sh 0.25s 0.5s 1s
#
# SETTING is Section.Key or a bare key in [Radio]. FIX holds other settings for the whole run
# (space-separated Section.Key=value). Everything touched is restored at
# the end.
#
#   NORESTART=1 COUNT=30 ./sms-burst-test.sh now      # one burst, settings as they are
#   NORESTART=1 LEN=50 ./sms-burst-test.sh len50       # messages padded to 50 characters
#
# With NORESTART=1 nothing is changed or restarted; the values only label
# the bursts.
#
# The radio must have transmitted through this hotspot (not to ECHO) within
# the last hour, so qtcd sends to it at once instead of holding messages.

set -euo pipefail

DEVICE=${DEVICE:-"N1ADJ 8"}
FROM=${FROM:-W1AW}
COUNT=${COUNT:-20}
ADMIN=${ADMIN:-127.0.0.1:8017}
SETTING=${SETTING:-PacketGap}
FIX=${FIX:-}
NORESTART=${NORESTART:-}
LEN=${LEN:-}
FILLER="the quick brown fox jumps over the lazy dog 0123456789 THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG"
INI=/etc/m17-gateway.ini
VALUES=("$@")

section_of() {
    case "$1" in
    *.*) echo "${1%%.*}" ;;
    *) echo Radio ;;
    esac
}
key_of() { echo "${1##*.}"; }

SECTION=$(section_of "$SETTING")
KEY=$(key_of "$SETTING")
if [ -n "$NORESTART" ]; then
    FIX=""
    [ ${#VALUES[@]} -gt 0 ] || VALUES=(now)
elif [ ${#VALUES[@]} -eq 0 ]; then
    case "$KEY" in
    PacketGap) VALUES=(0.25s 0.5s 1s 2s) ;;
    *) echo "give the values to try for $KEY" >&2; exit 2 ;;
    esac
fi

# get KEY: the current value, empty if unset.
get() { sudo sed -nE "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$INI" | tr -d '\r'; }

# set SECTION KEY VALUE: replace the key, or add it under the section.
set_key() {
    if sudo grep -qE "^[[:space:]]*$2[[:space:]]*=" "$INI"; then
        sudo sed -i -E "s|^[[:space:]]*$2[[:space:]]*=.*|$2 = $3|" "$INI"
    else
        sudo sed -i -E "/^\[$1\]/a $2 = $3" "$INI"
    fi
}

unset_key() { sudo sed -i -E "/^[[:space:]]*$1[[:space:]]*=/d" "$INI"; }

# Remember every key this run touches, and restore them on exit.
declare -A original
touched=("$SECTION.$KEY")
for f in $FIX; do touched+=("${f%%=*}"); done
for t in "${touched[@]}"; do original[$t]=$(get "$(key_of "$t")"); done
restore() {
    [ -z "$NORESTART" ] || return 0
    for t in "${touched[@]}"; do
        if [ -n "${original[$t]}" ]; then
            set_key "$(section_of "$t")" "$(key_of "$t")" "${original[$t]}"
        else
            unset_key "$(key_of "$t")"
        fi
    done
    sudo systemctl restart m17-gateway
    echo "Restored: ${touched[*]}"
}
trap restore EXIT

for f in $FIX; do
    t=${f%%=*}
    set_key "$(section_of "$t")" "$(key_of "$t")" "${f#*=}"
done

# wait_for UNIT SINCE PATTERN SECONDS: wait until UNIT's journal since SINCE
# has a line matching PATTERN.
wait_for() {
    local i
    for ((i = 0; i < $4; i++)); do
        if sudo journalctl -u "$1" --since "$2" --no-pager -o cat | grep -qE "$3"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

declare -A sent failed
for value in "${VALUES[@]}"; do
    if [ -z "$NORESTART" ]; then
        echo "=== $KEY $value${FIX:+ (with $FIX)}: restarting m17-gateway"
        set_key "$SECTION" "$KEY" "$value"
        start=$(date '+%Y-%m-%d %H:%M:%S')
        sudo systemctl restart m17-gateway
        if ! wait_for qtcd "$start" 'gateway relinked; radios reattached' 120; then
            echo "The gateway relinked, but qtcd did not reattach $DEVICE: key up once (not to ECHO) and rerun." >&2
            exit 1
        fi
        sleep 2
    else
        echo "=== $value: current settings"
    fi
    burst=$(date '+%Y-%m-%d %H:%M:%S')
    echo "    sending $COUNT messages to $DEVICE"
    for ((n = 1; n <= COUNT; n++)); do
        body=$(printf '%s %02d/%02d' "$value" "$n" "$COUNT")
        if [ -n "$LEN" ] && [ "${#body}" -lt "$LEN" ]; then
            pad=$FILLER
            while [ "${#pad}" -lt "$LEN" ]; do pad="$pad $FILLER"; done
            body="$body ${pad:0:$((LEN - ${#body} - 1))}"
        fi
        qtc send -admin "$ADMIN" -from "$FROM" -to "$DEVICE" -body "$body" >/dev/null
    done
    if wait_for qtcd "$burst" 'holding messages for device|no link to device; held' 3; then
        echo "qtcd is holding messages: $DEVICE is not in reach. Key up once (not to ECHO) and rerun." >&2
        exit 1
    fi
    last=$(printf '%s %02d/%02d' "$value" "$COUNT" "$COUNT")
    if ! wait_for m17-gateway "$burst" "Payload: $last[ ,]" 900; then
        echo "The gateway never sent \"$last\"; see journalctl -u m17-gateway." >&2
        exit 1
    fi
    log=$(sudo journalctl -u m17-gateway --since "$burst" --no-pager -o cat)
    failed[$value]=$(grep -c "Error transmitting packet" <<<"$log" || true)
    sent[$value]=$(( $(grep -cE "Payload: $value [0-9]+/" <<<"$log" || true) - failed[$value] ))
    echo "    gateway transmitted ${sent[$value]} of $COUNT (${failed[$value]} failed in the modem)"
    sleep 5 # let the radio finish storing before the next value
done

echo
echo "Gateway transmitted, per $KEY value (compare with what the radio decoded):"
for value in "${VALUES[@]}"; do
    printf '  %-8s %s of %s  (%s failed in the modem)\n' "$value" "${sent[$value]}" "$COUNT" "${failed[$value]}"
done
