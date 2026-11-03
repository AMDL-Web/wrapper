# wrapper-manager

> **AMDL-Web fork, in [AMDL-Web/wrapper](https://github.com/AMDL-Web/wrapper) under `manager/`.**
> Identical to
> [upstream `v2`](https://github.com/WorldObservationLog/wrapper-manager/tree/v2)
> except that it installs wrapper-lite from
> [AMDL-Web/wrapper's `lite` branch](https://github.com/AMDL-Web/wrapper/tree/lite),
> which frees the FairPlay decrypt context of each `/key`; upstream's build fails
> every `/key` with `KDCanProcessCKC status: -42786` after 1024 per process. A
> payload installed from anywhere else (no matching `data/wrapper/.payload-source`)
> is replaced on the next start; account data is kept. Upstream changes are merged
> in, not rebased.

An HTTP management/proxy gateway that runs and supervises **multiple
[wrapper-lite](https://github.com/WorldObservationLog/wrapper) account
instances** (branch `lite`) behind one HTTP endpoint.

Each logged-in account is a long-lived `wrapper-lite-rootless` child process
(service mode) listening on a private random port. wrapper-manager starts,
logs in, logs out, restarts and restores those instances, and exposes a
wrapper-lite-compatible HTTP API that aggregates them: a request carrying an
`adamId` is forwarded to an instance whose Apple storefront region can serve
that item, and the wrapper-lite response envelope is passed through unchanged.

> **Not a decryptor.** This project no longer provides sample decryption and no
> longer speaks gRPC. Decryption is performed locally by
> [AppleMusicDecrypt](https://github.com/WorldObservationLog/AppleMusicDecrypt)
> v3 (Temari); wrapper-manager only supplies the `/key` `ctx`/`state` template
> and `/m3u8`/`/license` upstreams that the decryptor consumes over HTTP.

## Features

- **Multi-account management** — every account = one instance, up to 10 per
  manager; requests are routed to the instance whose storefront region can
  serve the requested `adamId` (region availability is probed against the
  Apple catalog).
- **wrapper-lite-compatible HTTP API** — `/m3u8`, `/key`, `/lyrics`,
  `/webplayback`, `/license`, plus an aggregated `/status`.
- **`POST /login` / `POST /logout`** management endpoints, including a
  **two-phase 2FA login** flow.
- **Crash auto-restart** — when a service process exits it is relaunched and
  awaited until `/status` reports a region again (unless the account was
  logged out).
- **Startup recovery** — persisted accounts in `data/instances.json` are
  relaunched automatically (service mode only, no re-login).
- **First-run self-provisioning** — downloads the native wrapper-lite payload
  from nightly.link automatically when `data/wrapper/` is missing or was
  installed from another source.

## Requirements

- Linux **x86_64**. No native arm64 payload is built; on arm64 use the QEMU
  variant below.
- `wrapper-lite-rootless` needs a **user namespace** (`unshare`) and
  **chroot**. Running inside a container therefore requires the container to
  run with `--privileged` (or with a seccomp/apparmor profile that permits
  user namespaces); see [Deploy](#deploy-docker-compose). With `-launcher
  root` the manager uses `wrapper-lite` instead, which needs root (it mounts
  and chroots itself) but no user namespace.
- Internet access to `nightly.link` on first run (payload download) and to
  Apple services (`amp-api.music.apple.com`, etc.) for login, region probing
  and upstream requests. A configured `-proxy` applies to all of them.

## Build & run

```shell
go build -o wrapper-manager .
./wrapper-manager
```

On first start the manager downloads and unpacks the wrapper-lite native
package for your architecture from
`https://nightly.link/AMDL-Web/wrapper/workflows/build-lite/lite/wrapper-lite-linux-x86_64.zip`.
It is stored under `data/wrapper/`, with the URL it came from in
`data/wrapper/.payload-source`. Use `-prepare` to perform only this download and exit
(handy for pre-seeding images).

### Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-host` | `localhost` | Host the HTTP server binds to (use `0.0.0.0` in containers). |
| `-port` | `8080` | Port of the HTTP server. |
| `-mirror` | `false` | Download wrapper-lite through `gh-proxy.com` instead of nightly.link (for users in China). |
| `-debug` | `false` | Enable debug output. |
| `-proxy` | *(empty)* | HTTP(S) proxy used by the manager for downloads/Apple probes and passed through to wrapper-lite (`--proxy`). |
| `-prepare` | `false` | Only download the required wrapper-lite payload, then exit. |
| `-launcher` | `rootless` | `rootless` starts each wrapper-lite with `wrapper-lite-rootless` (user namespaces); `root` uses `wrapper-lite`, which needs root and suits containers that refuse a user namespace's `/proc` mount, such as Cloudflare Containers. |
| `-payload-url` | *(empty)* | wrapper-lite payload zip to install instead of the newest `build-lite` artifact on `lite`, e.g. one run's `https://nightly.link/AMDL-Web/wrapper/actions/runs/<run>/wrapper-lite-linux-x86_64.zip`. |
| `-payload-sha256` | *(empty)* | Lowercase hex sha256 the `-payload-url` zip must have. A download that differs is refused, nothing is installed and the manager exits, even when another payload is already installed; the digest is part of `data/wrapper/.payload-source`, so a new digest reinstalls. Needs `-payload-url`. |
| `-admin-port` | `0` (off) | Port of the [admin API](#admin-api) on `-host`. Never expose it. |
| `-device-info` | `Music/5.0.2/Android/10/Pixel 8/7663314/en-US/en-US` | Optional Apple Music device-info string (`--device-info` pass-through to wrapper-lite). |

## HTTP API

All responses use the wrapper-lite envelope, always `Content-Type:
application/json`:

```json
{ "code": 0, "msg": "SUCCESS", "data": { ... } }
```

| `code` | Meaning |
|---|---|
| `0` | Success. |
| `2` | 2FA code required — used by `POST /login` only (`data.loginId` tells the client which pending login to complete). |
| `-1` | Failure; `msg` carries the reason (manager-generated errors use `data: null`). |

### Resource endpoints (wrapper-lite compatible, multi-account aggregated)

The manager picks an instance whose storefront region can serve the request,
forwards to it, and passes the lite response body (envelope) through
untouched. No instance can serve the item → `code:-1, msg:"no available
instance"` (retry later).

| Method | Path | Parameters | Notes |
|---|---|---|---|
| `GET` | `/m3u8` | `adamId` | Playlist for the item. |
| `GET` | `/key` | `adamId`, `uri` (required) | Key/decryption context — `data` carries the `ctx`/`state` template used by AppleMusicDecrypt v3 + local Temari. The prefetch URI `skd://itunes.apple.com/P000000000/s1/e1` is only accepted with `adamId=0` (upstream rejects it for a real track). |
| `GET` | `/lyrics` | `adamId`, `language?`, `syllable?`, `script?` | Prefers an instance whose storefront supports the language and has lyrics (probed), falls back to region selection. `script` (transliteration, default `en-Latn`, e.g. `ja-Latn`) is forwarded to the instance. |
| `GET` | `/webplayback` | `adamId` | Web-playback session data. |
| `POST` | `/license` | JSON body `{"adamId","challenge","uri","drm-type"?}` | License request forwarded to the selected instance. Optional `drm-type`: `wv` (Widevine, default) or `pr` (PlayReady). |
| `GET` | `/status` | — | Aggregated status plus one entry per account: `data = {"status","regions","clientCount","ready","degraded","instances"}`. |
| `GET` | `/token` | `instance?`, `region?` | An account's music token from wrapper-lite's `/token`, forwarded from the instance with id `instance` (`region` is then ignored), else the instance serving `region`, else the first logged-in one: `data = {"region","music_token","refreshed_at","next_refresh_at"}`. HTTP 503 `{"code":503,"msg":…}` while that account has no token, has not come up, or does not exist. Never cached. Anyone who can reach this port can read the token: keep it off public networks. |

`/status` fields:

- `status` — whether at least one instance is currently registered/running.
- `regions` — the deduplicated storefront regions of all running instances.
- `clientCount` — number of running (logged-in) instances.
- `ready` — whether the manager has finished restoring the accounts persisted
  in `instances.json` (`true` immediately when there are none).
- `degraded` — how many instances are excluded for now after repeated
  FairPlay failures.
- `instances` — every registered account, in registry order, as
  `{"id","region","name","ready","degraded"}`: `id` is the instance id
  (`loginId`), `name` the Apple ID it logged in with (`""` for accounts logged
  in before the manager recorded it), `ready` whether its wrapper-lite is up
  (`region` is `""` until then), and `degraded` whether it is excluded after
  repeated FairPlay failures. Two accounts in one region both appear here,
  while `regions` lists the region once.

### Management endpoints

| Method | Path | Body | Behavior |
|---|---|---|---|
| `POST` | `/login` | `{"username","password"}` or `{"username","password","code"}` | Two-phase login, see below. |
| `POST` | `/logout` | `{"username"}` or `{"id"}` | Kill the account's service process, delete its data dir, remove it from `instances.json`, return `code:0`. `id` is the instance id and wins when both are sent. Unknown account → `code:-1, "no such account"`. |

**Two-phase 2FA login:**

1. `POST /login` with `{username, password}` starts the one-shot
   `wrapper-lite-rootless --login` child (`wrapper-lite` with `-launcher root`).
   - No 2FA needed: the request returns when the login settles →
     `code:0`, `data: {"loginId": "<id>"}` (tokens cached, service instance
     started and ready).
   - 2FA needed: returns as soon as lite prompts →
     `code:2, msg:"2fa code require"`, `data: {"loginId": "<id>"}`. The child
     keeps running and polls `<instance dir>/2fa.txt` for the code.
2. Re-`POST /login` with `{username, password, code}`. The manager writes the
   code to `2fa.txt`; this request blocks until the login completes and the
   service instance is ready → `code:0`, or `code:-1` on failure.

Notes:

- `/login` is idempotent per account: an account that is already logged in
  (registered with a region) returns `code:-1, "already login"`.
- A manager holds at most 10 accounts, counting logins in progress. A login
  for another account beyond that returns `code:-1, "account limit reached:
  this manager holds at most 10 accounts, log one out first"`.
- Sending a `code` with no pending login returns `code:-1` with a message
  telling you to restart the login without a code.
- Failed logins clean up the instance data directory before reporting.
- The instance id is a deterministic UUIDv5 derived from the username, so the
  `loginId` is stable across attempts.

### Admin API

For a host that keeps the accounts elsewhere between runs, such as a
container whose disk is lost when it sleeps. It listens on `-admin-port` only
and has no authentication; never expose it, since a snapshot holds every
account's session and tokens.

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/snapshot` | A gzipped tar of `data/instances.json` and the data dirs of the accounts it lists (file mtimes kept, `*.tmp` and `2fa.txt` left out). |
| `POST` | `/restore` | Body: a snapshot. Only while the manager holds no account: unpacks it into a staging dir (entries outside the registry and account dirs, links, a broken registry or more than 1 GiB unpacked fail the whole restore and change nothing), moves the registry and the dirs of the accounts it lists into place, starts them and answers once they are up, at most 90 s: `data = {"restored","ready"}`. |
| `GET` | `/stats` | Resident memory in bytes: `data = {"manager_rss","accounts":[{"id","region","rss"}]}`, where an account's `rss` sums its launcher and wrapper-lite. |

### Examples

```shell
# Aggregated status
curl -s http://localhost:8080/status
# {"code":0,"msg":"SUCCESS","data":{"status":false,"regions":[],"clientCount":0,"ready":true,"degraded":0,"instances":[]}}

# Start a login (this account requires 2FA)
curl -s -X POST http://localhost:8080/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"you@example.com","password":"hunter2"}'
# {"code":2,"msg":"2fa code require","data":{"loginId":"9f1a2b3c-...."}}

# Complete the 2FA login with the code
curl -s -X POST http://localhost:8080/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"you@example.com","password":"hunter2","code":"123456"}'
# {"code":0,"msg":"SUCCESS","data":{"loginId":"9f1a2b3c-...."}}

# Fetch a playlist once the instance is up
curl -s 'http://localhost:8080/m3u8?adamId=1608815075'
# {"code":0,"msg":"SUCCESS","data":{ ... lite envelope ... }}

# Log the account out
curl -s -X POST http://localhost:8080/logout \
  -H 'Content-Type: application/json' \
  -d '{"username":"you@example.com"}'
# {"code":0,"msg":"SUCCESS","data":{"username":"you@example.com"}}
```

## Data layout

Everything lives under `data/` relative to the working directory:

```
data/
├── wrapper/                                    # wrapper-lite payload (auto-downloaded)
│   ├── wrapper-lite-rootless                   # rootless launcher (login + service entry)
│   ├── wrapper-lite
│   └── rootfs/                                 # chroot userland used by the launcher
│       ├── system/bin/lite                     # the actual lite binary
│       └── data/instances/<instance-id>/       # per-account state (= lite --base-dir)
│           ├── STOREFRONT_ID                   # login artifacts written by lite
│           ├── MUSIC_TOKEN
│           ├── token_cache.json
│           └── 2fa.txt                         # present while a 2FA login is pending
└── instances.json                              # persisted account registry (service mode)
```

- `<instance-id>` is the UUIDv5 of the account username; the same id is the
  manager's `loginId` and the directory name.
- `--base-dir` passed to wrapper-lite is `/data/instances/<id>` (a path inside
  the chroot), which maps to the host path above.
- `instances.json` records `id`/`region`/`port`/`name` of logged-in accounts
  (`name` is the Apple ID, stored in clear text); on
  startup every entry is relaunched in service mode and `/status` is polled
  until its `regions` is non-empty (readiness).

## Run on any OS via QEMU (wrapper-manager-qemu)

The native manager requires Linux (it launches `wrapper-lite-rootless`, which
needs user namespaces + chroot). To run it on **Windows / macOS / any Linux**
without a Linux host, boot the whole manager inside a single QEMU Linux guest:

```
Windows / macOS / Linux host
  └─ wrapper-manager-qemu (Go launcher, cross-platform)
       └─ QEMU x86_64 guest: vmlinuz-lite-qemu + wrapper-manager-initramfs
            └─ wrapper-manager -host 0.0.0.0 -port 8080   (Linux native)
                 └─ wrapper-lite-rootless instances (per account)
```

The launcher is modeled after upstream
[wrapper-lite-qemu](https://github.com/WorldObservationLog/wrapper/blob/lite/wrapper-lite-qemu.cpp):

- Locates `qemu-system-x86_64` (`--qemu-bin` > `QEMU_BIN` > `PATH` > bundled).
- Auto-selects acceleration: KVM (Linux) / WHPX (Windows) / TCG (macOS &
  fallback) with automatic fallback to TCG when the accel is unavailable.
- Boots the guest kernel + initramfs, attaches a persistent `data.img` and
  forwards `hostPort` → guest manager port (8080).
- Guest images live in `<exe-dir>/guest/` (`--assets-dir` to override); the
  persistent data image defaults to `~/.wrapper-manager/data.img`.

### Build

```shell
# 1. the launcher (any platform)
go build -o wrapper-manager-qemu ./cmd/wrapper-manager-qemu

# 2. the guest (on Linux; see guest/README.md). The kernel and its e1000 and
#    qemu_fw_cfg modules come from the build-lite qemu-assets artifact.
curl -fsSL -o qemu-assets.zip \
  "https://nightly.link/AMDL-Web/wrapper/workflows/build-lite/lite/qemu-assets.zip"
unzip -o qemu-assets.zip vmlinuz-lite-qemu lite-initramfs.cpio.gz -d kernel-assets/
KERNEL_ASSETS=kernel-assets ./guest/build.sh out                    # kernel + initramfs
KERNEL_ASSETS=kernel-assets CREATE_DATA_IMG=1 ./guest/build.sh out  # also a fresh data.img
```

Prebuilt artifacts (launchers for linux/windows/macos + guest image) are
published by the `build-manager-qemu` workflow and are fetchable through
nightly.link.

### Run

```shell
./wrapper-manager-qemu --host-port 8080
# guest boots; manager becomes available at http://127.0.0.1:8080
# (same HTTP API and /login /logout as the native run)
```

Flags: `--host --host-port --guest-port --memory --smp --accel --qemu-bin
--assets-dir --data-dir --kernel --initrd`; environment fallbacks
`QEMU_BIN / HOST_PORT / GUEST_PORT / MEMORY / SMP`.

Guest networking uses QEMU user-mode NAT (`10.0.2.15`); the guest downloads the
wrapper-lite payload itself on first boot (nightly.link) and keeps everything
inside `data.img`, so account state and payload survive restarts.

## Deploy (docker compose)

wrapper-lite-rootless creates **user namespaces** and calls **chroot**, which
Docker blocks by default — the compose file runs the container with
`privileged: true`. If you prefer not to use `privileged`, run with a
seccomp/apparmor profile that permits `unshare`/user namespaces instead.

```shell
git clone https://github.com/AMDL-Web/wrapper
cd wrapper/manager
docker compose up -d
```

- The HTTP API is published on `localhost:8080`.
- A named volume is mounted at `/root/data`, so account state and the
  wrapper-lite payload survive container restarts.
- The compose command passes `--host 0.0.0.0` (the binary default binds to
  `localhost`, which is useless inside a container).

For users in China: uncomment the Go module proxy line in `Dockerfile` and add
`--mirror` to the compose `command` to fetch wrapper-lite through gh-proxy.

## Related projects

- [WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper) — upstream wrapper / wrapper-lite (branch `lite`).
- [WorldObservationLog/AppleMusicDecrypt](https://github.com/WorldObservationLog/AppleMusicDecrypt) — decryptor that consumes this manager's `/m3u8` `/key` `/license` API.
