#!/usr/bin/env bash
# Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT
#
# Boots a built guest with wrapper-manager-qemu on two disks:
#   - none: the launcher creates a blank data.img, the guest formats it, the
#     manager installs its default payload and answers /status;
#   - a disk in the layout older managers wrote: its accounts are
#     listed with their ids and names and come up;
#   - an ext4 disk that does not mount: the guest leaves it untouched and
#     powers off by itself.
# Usage: LAUNCHER=<wrapper-manager-qemu> guest/disktest.sh <guest-dir>
# Needs qemu-system-x86_64, curl and sha256sum; KVM makes it quicker.
set -euo pipefail

guest=$(cd "${1:?usage: disktest.sh <guest-dir>}" && pwd)
launcher=${LAUNCHER:?set LAUNCHER to the wrapper-manager-qemu binary}
work=$(mktemp -d)
trap 'kill "$pid" 2>/dev/null || true; rm -rf "$work"' EXIT
pid=
fail() { echo "disktest: $*" >&2; exit 1; }

echo "== no data disk: formatted, payload installed, /status answers"
port=18080
"$launcher" --assets-dir "$guest" --data-dir "$work/blank" --host-port "$port" >"$work/blank.log" 2>&1 &
pid=$!
answered=
for _ in $(seq 1 120); do
    if curl -fsS --max-time 3 "http://127.0.0.1:$port/status" >"$work/status.json" 2>/dev/null; then
        answered=1
        break
    fi
    kill -0 "$pid" 2>/dev/null || break
    sleep 5
done
kill -TERM "$pid" 2>/dev/null || true
wait "$pid" || true
pid=
cat "$work/blank.log"
[ -n "$answered" ] || fail "the guest on a blank disk never answered /status"
cat "$work/status.json"; echo
grep -q '\[init\] the data disk is blank; formatting it' "$work/blank.log" || fail "the blank disk was not formatted"
grep '\[init\] data: ' "$work/blank.log" | grep -q 'commit=1' || fail "the formatted disk is not mounted with commit=1"
grep -q 'wrapper-lite ready' "$work/blank.log" || fail "the default payload was not installed"

echo "== a disk in the layout older managers wrote: its accounts are served"
a=b193ebc0-ca20-50a7-b158-6a8111b2a0f3 # a@example.com
b=76249329-7b1f-51f9-a084-66764794a402 # b@example.com
root=$work/oldroot
mkdir -p "$root/data"
cat >"$root/data/instances.json" <<EOF
[
  {"id": "$a", "region": "us", "port": 41234},
  {"id": "$b", "region": "jp", "port": 41235, "name": "b@example.com"}
]
EOF
for pair in "$a us" "$b jp"; do
    set -- $pair
    dir=$root/data/wrapper/rootfs/data/instances/$1
    mkdir -p "$dir/mpl_db"
    printf '%s' "$2" >"$dir/STOREFRONT_ID"
    printf 'not-a-real-token' >"$dir/MUSIC_TOKEN"
    printf '{"dev_token":"x","music_token":"not-a-real-token","storefront_id":"%s"}' "$2" >"$dir/token_cache.json"
done
chmod -R a+rwX "$root"
mkdir -p "$work/old"
truncate -s 512M "$work/old/data.img"
mke2fs -q -t ext4 -F -d "$root" "$work/old/data.img"
port=18082
"$launcher" --assets-dir "$guest" --data-dir "$work/old" --host-port "$port" >"$work/old.log" 2>&1 &
pid=$!
served=
for _ in $(seq 1 120); do
    if curl -fsS --max-time 3 "http://127.0.0.1:$port/status" >"$work/status.json" 2>/dev/null &&
        grep -q '"ready":true' "$work/status.json"; then
        served=1
        break
    fi
    kill -0 "$pid" 2>/dev/null || break
    sleep 5
done
kill -TERM "$pid" 2>/dev/null || true
wait "$pid" || true
pid=
grep -v -E '\[wrapper [0-9a-f]{8}\]' "$work/old.log" || true
cat "$work/status.json"; echo
[ -n "$served" ] || fail "the old disk's accounts never came up"
for want in "\"clientCount\":2" "\"id\":\"$a\",\"region\":\"us\",\"name\":\"\",\"ready\":true" \
    "\"id\":\"$b\",\"region\":\"jp\",\"name\":\"b@example.com\",\"ready\":true"; do
    grep -qF "$want" "$work/status.json" || fail "/status lacks $want"
done

echo "== an ext4 disk that does not mount: left untouched, guest powers off"
mkdir -p "$work/bad"
cp "$guest/data.img" "$work/bad/data.img"
# Every unknown incompatible feature bit set: ext4 refuses to mount it.
printf '\xff\xff\xff\x7f' | dd of="$work/bad/data.img" bs=1 seek=$((1024 + 0x60)) conv=notrunc status=none
before=$(sha256sum <"$work/bad/data.img")
timeout 300 "$launcher" --assets-dir "$guest" --data-dir "$work/bad" --host-port $((port + 1)) >"$work/bad.log" 2>&1 \
    || { cat "$work/bad.log"; fail "the guest did not power off by itself"; }
cat "$work/bad.log"
grep -q 'leaving it untouched and powering off' "$work/bad.log" || fail "no refusal line"
[ "$(sha256sum <"$work/bad/data.img")" = "$before" ] || fail "the disk was changed"
echo "disktest: ok"
