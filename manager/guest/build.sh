#!/usr/bin/env bash
# Copyright (c) 2026 AMDL-Web contributors. SPDX-License-Identifier: MIT
#
# Builds the wrapper-manager QEMU guest that amdl boots:
#   vmlinuz-lite-qemu                  the build-lite kernel, unchanged
#   wrapper-manager-initramfs.cpio.gz  init, busybox, the manager, two modules,
#                                      the glibc the launcher needs, mke2fs,
#                                      CA bundle
#   data.img                           with CREATE_DATA_IMG=1: an empty ext4 disk
#
# Usage: KERNEL_ASSETS=<unzipped qemu-assets> guest/build.sh [out-dir]
# Run from anywhere; needs Linux x86_64, go, cpio, gzip, file, mke2fs and a
# static busybox (BUSYBOX, else busybox on PATH).
set -euo pipefail

manager=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-dist-guest}
mkdir -p "$out"
out=$(cd "$out" && pwd)
fail() { echo "build.sh: $*" >&2; exit 1; }

: "${KERNEL_ASSETS:?set KERNEL_ASSETS to the unzipped qemu-assets artifact of build-lite}"
[ -d "$KERNEL_ASSETS" ] || fail "$KERNEL_ASSETS is not a directory"
KERNEL_ASSETS=$(cd "$KERNEL_ASSETS" && pwd)
kernel=$KERNEL_ASSETS/vmlinuz-lite-qemu
lite_initrd=$KERNEL_ASSETS/lite-initramfs.cpio.gz
[ -f "$kernel" ] || fail "$kernel is missing"
[ -f "$lite_initrd" ] || fail "$lite_initrd is missing"

busybox=${BUSYBOX:-$(command -v busybox || true)}
[ -n "$busybox" ] && [ -x "$busybox" ] || fail "no busybox; install busybox-static or set BUSYBOX"
file -L "$busybox" | grep -q 'statically linked' || fail "$busybox is not statically linked"
for applet in mkdir mount insmod ip sleep usleep cat dd od tr grep nslookup mke2fs sync umount poweroff chmod hostname head timeout ln; do
    "$busybox" --list | grep -qx "$applet" || fail "$busybox has no $applet applet"
done

stage=$(mktemp -d)
mods=$(mktemp -d)
trap 'rm -rf "$stage" "$mods"' EXIT
mkdir -p "$stage/bin" "$stage/lib64" "$stage/lib/x86_64-linux-gnu" "$stage/etc/ssl/certs" \
    "$stage/data" "$stage/proc" "$stage/sys" "$stage/dev" "$stage/tmp"

# The two modules, taken from the build-lite initramfs, built for its kernel.
(cd "$mods" && gzip -dc "$lite_initrd" | cpio -id --quiet '*e1000.ko' '*qemu_fw_cfg.ko')
kver=$(file -b "$kernel" | sed -n 's/.* version \([^ ]*\) .*/\1/p')
[ -n "$kver" ] || fail "cannot read the kernel version of $kernel"
for mod in e1000 qemu_fw_cfg; do
    ko=$(find "$mods" -name "$mod.ko" | head -n 1)
    [ -n "$ko" ] || fail "$lite_initrd has no $mod.ko"
    magic=$(tr '\0' '\n' < "$ko" | sed -n 's/^vermagic=\([^ ]*\).*/\1/p' | head -n 1)
    [ "$magic" = "$kver" ] || fail "$mod.ko is built for kernel '$magic', but the kernel is '$kver'"
    install -m 0644 "$ko" "$stage/$mod.ko"
done

install -m 0755 "$manager/guest/init" "$stage/init"
install -m 0755 "$busybox" "$stage/bin/busybox"
(cd "$manager" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o "$stage/wrapper-manager" .)

# wrapper-lite-rootless is linked against glibc; the initramfs has no other libc.
glibc=${GLIBC_ROOT:-}
cp -L "$glibc/lib64/ld-linux-x86-64.so.2" "$stage/lib64/"
cp -L "$glibc/lib/x86_64-linux-gnu/libc.so.6" "$stage/lib/x86_64-linux-gnu/"
cp -L /etc/ssl/certs/ca-certificates.crt "$stage/etc/ssl/certs/"

# e2fsprogs' mke2fs and its libraries: busybox formats only ext2, and a blank
# disk must become ext4 with a journal to mount with commit=1.
mkfs=$(command -v mke2fs) || fail "no mke2fs; install e2fsprogs"
install -m 0755 -D "$(readlink -f "$mkfs")" "$stage/sbin/mke2fs"
for lib in $(ldd "$mkfs" | awk '$2 == "=>" && $3 ~ /^\// { print $3 }'); do
    install -m 0644 -D "$(readlink -f "$lib")" "$stage$lib"
done

(cd "$stage" && find . -mindepth 1 | LC_ALL=C sort | cpio -o -H newc --quiet -R 0:0) | gzip -9 > "$out/wrapper-manager-initramfs.cpio.gz"
cp "$kernel" "$out/vmlinuz-lite-qemu"

if [ "${CREATE_DATA_IMG:-}" = 1 ]; then
    rm -f "$out/data.img"
    truncate -s 512M "$out/data.img"
    mke2fs -q -t ext4 -F "$out/data.img"
fi
ls -l "$out"
