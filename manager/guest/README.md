# wrapper-manager QEMU guest

Assets for booting the whole wrapper-manager inside a QEMU Linux guest so it
runs on Windows / macOS / any Linux host (see the parent README).

## Layout

```text
guest/
├── build.sh     # builds wrapper-manager-initramfs.cpio.gz (+ optional data.img)
└── init         # guest init script (busybox sh)
```

`overlay/` is a build output (gitignored): build.sh rebuilds it from scratch
each run by staging the static wrapper-manager binary, static busybox, the
host glibc runtime (for the dynamically-linked `wrapper-lite-rootless`), CA
certificates and the kernel modules.

## Build

```shell
# requires: go, cpio, gzip, e2fsprogs, file, kmod, glibc + ca-certificates (any Debian/Ubuntu)
curl -fsSL -o qemu-assets.zip \
  "https://nightly.link/AMDL-Web/wrapper/workflows/build-lite/lite/qemu-assets.zip"
unzip -o qemu-assets.zip vmlinuz-lite-qemu lite-initramfs.cpio.gz -d kernel-assets/
KERNEL_ASSETS=kernel-assets ./guest/build.sh out                    # kernel + initramfs
KERNEL_ASSETS=kernel-assets CREATE_DATA_IMG=1 ./guest/build.sh out  # also a fresh 512MB data.img
```

## Kernel

`vmlinuz-lite-qemu` is **not** built here. It is the kernel produced by the
[AMDL-Web/wrapper](https://github.com/AMDL-Web/wrapper) (a fork of
[WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper))
`build-lite` workflow and is distributed in its `qemu-assets` artifact.
build.sh copies it next to the initramfs, and takes `e1000.ko` and
`qemu_fw_cfg.ko` from the same artifact's `lite-initramfs.cpio.gz`: modules
built for another kernel do not load, which leaves the guest without network
and without its fw_cfg arguments. build.sh fails when their vermagic differs
from the kernel's version.

## What the guest does on boot (init)

1. Mounts proc/sys/dev, loads `e1000.ko`, configures static NAT networking
   (10.0.2.15, DNS 10.0.2.3) and writes `/etc/resolv.conf`.
2. Mounts the ext4 data disk (`/dev/vda` → `/data`); formats it with
   `mke2fs` on first boot when it is a raw empty image.
3. Runs `/wrapper-manager -host 0.0.0.0 -port 8080` from `/data`, so all
   manager state (wrapper-lite payload, accounts, tokens) persists in
   `data.img`. Optional manager args can be supplied via qemu fw_cfg
   (`manager_args`) or the kernel cmdline (`manager_args_b64=`).
4. Powers the guest off when the manager exits, so QEMU exits with it rather
   than hanging on a kernel panic. The manager itself retries a failed
   wrapper-lite download with backoff instead of exiting; only a download
   that fails its pinned digest ends it.
