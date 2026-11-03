# wrapper-manager

Runs one wrapper-lite process per Apple Music account (up to 10) behind one HTTP
port: login with two-step codes, account supervision, and the resource routes
(`/m3u8`, `/key`, `/lyrics`, `/webplayback`, `/license`, `/token`) routed to an
account whose storefront has the song. MIT licensed, like the rest of this
repository. Standard library Go only.

```sh
CGO_ENABLED=0 go build -o wrapper-manager .
./wrapper-manager -host 0.0.0.0 -port 8080
```

It keeps everything in `data/` under its working directory: the wrapper-lite
payload in `data/wrapper/`, the account registry `data/instances.json`, and one
directory per account in `data/wrapper/rootfs/data/instances/<id>/`. On first
start it downloads and verifies the payload, a pinned wrapper-lite release of
this repository.

| Flag | Default | |
| --- | --- | --- |
| `-host`, `-port` | `localhost`, `8080` | listen address |
| `-launcher` | `rootless` | `rootless` needs user namespaces; `root` needs root |
| `-payload-url`, `-payload-sha256` | the pinned release | install another payload zip (a digest needs a URL) |
| `-proxy` | | proxy for the manager's own requests and every wrapper-lite |
| `-prepare` | | install the payload and exit |
| `-update-wrapper` | | download and install the payload again and exit |
| `-admin-port` | `0` (off) | snapshot and restore API; never expose it |

## Docker

```sh
docker build -t wrapper-manager .
docker run --privileged -v wrapper-data:/root/data -p 8080:8080 wrapper-manager --host 0.0.0.0 --port 8080
```

The container needs `unshare` (privileged, or a seccomp profile that allows it).

## Run on any OS via QEMU (wrapper-manager-qemu)

Where user namespaces are missing (Synology DSM, macOS, Windows), run the manager
inside a small QEMU guest. `guest/build.sh` builds the guest (kernel, initramfs,
`data.img`); `cmd/wrapper-manager-qemu` boots it with KVM, WHPX or plain
emulation, keeps the accounts in `<data-dir>/data.img` and forwards a host port
to the manager:

```sh
wrapper-manager-qemu --host 0.0.0.0 --host-port 8080 --assets-dir /guest --data-dir /data
```

`Dockerfile.qemu` packages the launcher, QEMU and a built guest (put the guest
in `dist-guest/` first); the container needs `/dev/kvm` and nothing else. amdl's
desktop app and `amdl serve --local` boot the same guest with their own
launcher.
