#!/usr/bin/env bash
# Build wrapper-manager-initramfs.cpio.gz (and optionally a fresh data.img)
# for the wrapper-manager QEMU guest.
#
# Requirements (build machine must be Linux x86_64):
#   - go
#   - glibc runtime (any Debian/Ubuntu host has it at the standard paths)
#   - CA certificates (/etc/ssl/certs/ca-certificates.crt)
#   - busybox-static (auto-downloaded via apt if missing)
#   - cpio, gzip, e2fsprogs (mke2fs)
#
# The kernel (vmlinuz-lite-qemu) is NOT built here - it is reused from the
# build-lite qemu-assets artifact (see guest/README.md). KERNEL_ASSETS names
# the unzipped artifact: the kernel and the e1000 / qemu_fw_cfg modules both
# come from it, so they always match. A module built for another kernel fails
# to load, and the guest boots without network or fw_cfg arguments.
set -euo pipefail

KERNEL_ASSETS="${KERNEL_ASSETS:?set KERNEL_ASSETS to the unzipped build-lite qemu-assets artifact}"
KERNEL_ASSETS="$(cd "$KERNEL_ASSETS" && pwd)"

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
GUEST_DIR="$REPO_DIR/guest"
OVERLAY="$GUEST_DIR/overlay"
OUT_DIR="${1:-$GUEST_DIR/out}"

# Normalize OUT_DIR to an absolute path: the initramfs step runs inside a
# subshell that cd's into the overlay, so a relative output path would resolve
# against the wrong directory and fail.
mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd)"

echo "[build] output dir: $OUT_DIR"
rm -rf "$OVERLAY"
mkdir -p "$OVERLAY/bin" "$OVERLAY/etc/ssl/certs" "$OVERLAY/lib64" \
         "$OVERLAY/lib/x86_64-linux-gnu"

# 1. Static wrapper-manager binary.
echo "[build] building static wrapper-manager..."
(cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux go build -o "$OVERLAY/wrapper-manager" .)

# 2. Static busybox.
if [ ! -x "$OVERLAY/bin/busybox" ]; then
    if [ -x /bin/busybox ] && file /bin/busybox 2>/dev/null | grep -q statically; then
        echo "[build] using system static busybox"
        cp /bin/busybox "$OVERLAY/bin/busybox"
    else
        echo "[build] downloading busybox-static..."
        (cd /tmp && apt-get download busybox-static >/dev/null 2>&1)
        dpkg-deb -x /tmp/busybox-static_*.deb /tmp/busybox-static-extract
        cp /tmp/busybox-static-extract/bin/busybox "$OVERLAY/bin/busybox"
        rm -rf /tmp/busybox-static-extract
    fi
fi
chmod +x "$OVERLAY/bin/busybox"

# 3. glibc runtime for wrapper-lite-rootless (dynamically linked launcher).
echo "[build] staging glibc..."
for f in /lib/x86_64-linux-gnu/libc.so.6 /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2; do
    [ -f "$f" ] || { echo "missing $f"; exit 1; }
done
cp /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 "$OVERLAY/lib64/ld-linux-x86-64.so.2"
cp /lib/x86_64-linux-gnu/libc.so.6 "$OVERLAY/lib/x86_64-linux-gnu/libc.so.6"

# 4. CA certificates (for HTTPS downloads inside the guest).
echo "[build] staging CA certificates..."
if [ ! -f "$OVERLAY/etc/ssl/certs/ca-certificates.crt" ]; then
    if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
        cp /etc/ssl/certs/ca-certificates.crt "$OVERLAY/etc/ssl/certs/"
    else
        echo "missing CA bundle; install ca-certificates"
        exit 1
    fi
fi

# 5. Kernel and the modules built for it, from the same qemu-assets artifact.
echo "[build] staging the kernel and its modules..."
KERNEL_VER="$(file -bL "$KERNEL_ASSETS/vmlinuz-lite-qemu" | sed -n 's/.*version \([^ ]*\).*/\1/p')"
[ -n "$KERNEL_VER" ] || { echo "cannot read the version of $KERNEL_ASSETS/vmlinuz-lite-qemu"; exit 1; }
LITE_ROOT="$(mktemp -d)"
trap 'rm -rf "$LITE_ROOT"' EXIT
(cd "$LITE_ROOT" && gzip -dc "$KERNEL_ASSETS/lite-initramfs.cpio.gz" | cpio -id --quiet)
for mod in e1000 qemu_fw_cfg; do
    [ -f "$LITE_ROOT/$mod.ko" ] || { echo "$mod.ko is missing from the qemu-assets initramfs"; exit 1; }
    cp "$LITE_ROOT/$mod.ko" "$OVERLAY/$mod.ko"
    magic="$(modinfo -F vermagic "$OVERLAY/$mod.ko" | cut -d' ' -f1)"
    [ "$magic" = "$KERNEL_VER" ] || { echo "$mod.ko is built for $magic, the kernel is $KERNEL_VER"; exit 1; }
done
cp "$KERNEL_ASSETS/vmlinuz-lite-qemu" "$OUT_DIR/vmlinuz-lite-qemu"
echo "[build] kernel and modules: $KERNEL_VER"

# 6. Init script at the initramfs root (required by the kernel).
cp "$GUEST_DIR/init" "$OVERLAY/init"
chmod +x "$OVERLAY/init"

# 7. Assemble the initramfs cpio archive.
echo "[build] assembling initramfs..."
(
    cd "$OVERLAY"
    find . | cpio -o -H newc 2>/dev/null | gzip -9 > "$OUT_DIR/wrapper-manager-initramfs.cpio.gz"
)
ls -la "$OUT_DIR/wrapper-manager-initramfs.cpio.gz"

# 8. Fresh empty data.img (512MB ext4) - only when explicitly wanted.
if [ "${CREATE_DATA_IMG:-0}" = "1" ]; then
    echo "[build] creating fresh data.img..."
    DATA_IMG="$OUT_DIR/data.img"
    rm -f "$DATA_IMG"
    truncate -s 512M "$DATA_IMG"
    mke2fs -q -t ext4 -F "$DATA_IMG"
    echo "[build] data.img created: $DATA_IMG"
fi

echo "[build] done."
