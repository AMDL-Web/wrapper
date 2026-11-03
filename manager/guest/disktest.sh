#!/usr/bin/env bash
# Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT
#
# Boots a built guest with wrapper-manager-qemu on two disks:
#   - none: the launcher creates a blank data.img, the guest formats it, the
#     manager installs its default payload and answers /status;
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
grep -q 'wrapper-lite ready' "$work/blank.log" || fail "the default payload was not installed"

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
