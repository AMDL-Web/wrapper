# AMDL wrapper

Holds an Apple Music account session for [amdl](https://github.com/AMDL-Web/apple-music-downloader).
amdl asks it for playlists, decryption keys, lyrics, web-playback data, licenses
and the account's music token over HTTP (amdl's `wrapper.address`); it does the
Apple-side work amdl cannot do itself.

This repository is AMDL-Web's fork of
[WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper).
Its work lives on the `amdl` branch, rebased on upstream's `lite`; the `lite`
branch mirrors upstream.

| Part | Directory | What it does |
| --- | --- | --- |
| **wrapper-lite** | this root (`lite/`, launchers, `rootfs/`, `qemu/`) | Upstream's wrapper-lite with [a few patches](#what-differs-from-upstream): runs Apple Music's Android libraries for one logged-in account and serves the [HTTP API](#http-api) on one port. |
| **wrapper-manager** | [`manager/`](manager/) | Starts, logs in, restarts and restores up to 10 wrapper-lite instances behind one wrapper-lite-compatible port, with `POST /login` (two-step codes) and `POST /logout`. |
| **Manager guest** | [`manager/guest/`](manager/guest/), [`manager/cmd/wrapper-manager-qemu`](manager/cmd/wrapper-manager-qemu/) | A small QEMU guest that runs the manager where user namespaces are missing (macOS, Windows, Synology DSM). amdl's desktop app boots it. |
| **Cloudflare template** | [`cloudflare/`](cloudflare/) | One-click deploy of the manager on Cloudflare Containers, behind a static key, with an encrypted backup in R2. |

A bare wrapper-lite can serve downloads. The manager adds the login API that
amdl's console and desktop app use to sign accounts in. Neither has
authentication: keep its port on the machines that need it (`GET /token`
returns the account's music token). The Cloudflare deployment adds a key.

## Run it

**amdl desktop app.** Nothing to do here. The app downloads the manager guest,
pinned by digest, and runs it on this machine.

**Docker, or a Linux host.** Run the manager, then log in from amdl's console.

```shell
git clone -b amdl https://github.com/AMDL-Web/wrapper
cd wrapper/manager
docker build -t wrapper-manager .
docker run -d --privileged -v wrapper-data:/root/data -p 127.0.0.1:8080:8080 \
  wrapper-manager --host 0.0.0.0 --port 8080
```

Without Docker (Linux x86_64, user namespaces and chroot available):

```shell
cd wrapper/manager
CGO_ENABLED=0 go build -o wrapper-manager .
./wrapper-manager            # binds localhost:8080; -host sets the address
```

The API has no authentication, so keep 8080 off networks you do not trust. On
first start the manager downloads the wrapper-lite release it pins, checks its
sha256 and installs it into `data/wrapper/`. Flags and the QEMU launcher are in
[`manager/README.md`](manager/README.md).

**Cloudflare.** One-click deploy of the manager behind a static key, with an
encrypted backup in R2 and a daily token renewal. See
[`cloudflare/README.md`](cloudflare/README.md).

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/AMDL-Web/wrapper/tree/amdl/cloudflare)

Then set the address (and key, for Cloudflare) in amdl's Settings; the amdl docs
cover [the wrapper](https://github.com/AMDL-Web/apple-music-downloader/blob/dev/docs/install.md#the-wrapper)
and [its configuration](https://github.com/AMDL-Web/apple-music-downloader/blob/dev/docs/configuration.md#wrapper).

## Release assets

Every successful push build on `amdl` is published as a GitHub pre-release
([`publish.yml`](.github/workflows/publish.yml)); an asset is never replaced
once published, so its sha256 can be pinned:

| Tag | Asset | From |
| --- | --- | --- |
| `guest-<sha12>` | `manager-qemu-guest.zip` | `manager.yml`: the manager guest (`manager/guest/`) |
| `lite-<sha12>` | `wrapper-lite-linux-x86_64.zip` | `build-lite.yml`: the native payload the manager installs |

`<sha12>` is the first 12 characters of the commit. The native payload carries
22 of the 98 libraries in `rootfs/system/lib64`, the ones wrapper-lite loads;
`tools/slim-rootfs.py` removes the rest when the package is assembled, and its
`KEEP` list says why each stays. The repository's `rootfs/` stays whole.

A change under `manager/`, `cloudflare/` or a `.md` file alone does not run
`build-lite`.

## What differs from upstream

wrapper-lite on `amdl` is upstream's `lite` plus:

- **`GET /token`**, with `refreshed_at` and `next_refresh_at`. Logs print token
  lengths, not token prefixes.
- **Stale token refresh at start.** A process that starts when the cached token
  is older than `--token-refresh-interval` refreshes it a few seconds in, not a
  full interval later (a woken container, a machine back from a restart).
- **`APPLE_ID` file and `/status` `name`.** Login records the Apple ID in
  `<base-dir>/APPLE_ID`; `/status` reports it so clients can label the account.
- **Root launcher in a private mount namespace.** `wrapper-lite` run as root
  mounts in its own namespace instead of the host's.
- **Slimmed native payload** (see above) and `macos-15` in `build-lite` (GitHub
  retired `macos-14`).

The fix that frees each FairPlay decrypt context after `/key` and fetches the
persistent key once per `/key` started here and is upstream now
(WorldObservationLog/wrapper#78, #79).

Everything else (`manager/`, `cloudflare/`, `tools/`, `publish.yml`,
`manager.yml`) exists only in this fork.

## wrapper-lite reference

### Build

```bash
mkdir -p build && cd build
cmake .. -DCMAKE_BUILD_TYPE=Release
make -j$(nproc)
```

Debug build:

```bash
cmake .. -DCMAKE_BUILD_TYPE=Debug
```

Build outputs:

- `rootfs/system/bin/lite` — Android-layer binary
- `wrapper-lite` — privileged host launcher
- `wrapper-lite-rootless` — rootless host launcher (user namespaces)

### Run natively

```bash
# Login, cache tokens, then exit
./wrapper-lite-rootless --login user:pass --code-from-file --base-dir /data

# Start the HTTP service
./wrapper-lite-rootless --base-dir /data --host 0.0.0.0 --port 12340
```

2FA code:

- Interactive prompt when a TTY is available.
- With `--code-from-file`, the code is read from `data/2fa.txt`.

### Run with QEMU

`wrapper-lite-qemu` boots the prebuilt guest
(the `qemu/` assets) in QEMU and forwards the guest's HTTP service to the host,
so no rooted device or native chroot is required. Packages that bundle QEMU
and its firmware put them in `qemu/bin/`.

```bash
# one-time: build the launcher
c++ -std=c++11 -O2 -o wrapper-lite-qemu wrapper-lite-qemu.cpp

# login first (forwards --login to the guest lite), then serve
./wrapper-lite-qemu --login user:pass --code-from-file
./wrapper-lite-qemu
```

The launcher locates the QEMU binary in this order:

1. `--qemu-bin <path>` / `QEMU_BIN`
2. `qemu/bin/` (bundled)
3. `PATH`

The bundled QEMU is preferred so that the firmware (`-L qemu/bin`), the QEMU
modules (`QEMU_MODULE_DIR`) and the support libraries (`qemu/lib`) always come
from the same package. `-L`, `LD_LIBRARY_PATH` and `QEMU_MODULE_DIR` are only
adjusted for the bundled QEMU; a system QEMU keeps its own libraries, modules
and firmware.

A package never ships glibc (`libc.so.6`, `libm.so.6`): the ELF interpreter is
always the host's and cannot be replaced through `LD_LIBRARY_PATH`, so the
loader and libc must stay a matched pair from the host. Only non-glibc support
libraries are bundled, in `qemu/lib/` (`qemu/bin/` on Android, where the Termux
packages place them next to the binary).

See [QEMU launcher arguments](#qemu-launcher-arguments) for the full option
list.

### Docker

```bash
docker build -t wrapper-lite:local .
docker run --privileged -p 12340:12340 \
  -v ./rootfs/data:/app/rootfs/data \
  -e USERNAME=... -e PASSWORD=... \
  wrapper-lite:local
```

### HTTP API

All responses use:

```json
{"code":0,"msg":"SUCCESS","data":{...}}
```

| Endpoint | Method | Parameters |
|----------|--------|------------|
| `/m3u8` | GET | `adamId` |
| `/key` | GET | `adamId`, `uri` (required; the prefetch `skd://itunes.apple.com/P000000000/s1/e1` is rejected unless `adamId=0`) |
| `/lyrics` | GET | `adamId`, optional `language`, optional `syllable` (`1`=word-timed syllable-lyrics with translations/romanization, `0`=line-timed lyrics converted in-wrapper **and still carrying translations/romanization as whole-line text**), optional `script` (transliteration script, default `en-Latn`) |
| `/webplayback` | GET | `adamId` |
| `/license` | POST | JSON: `adamId`, `challenge`, `uri`, optional `drm-type` (`wv`=Widevine (default), `pr`=PlayReady) |
| `/status` | GET | returns `regions` (list of storefront codes this wrapper can serve) and `name` (the Apple ID the account logged in with, from `<base-dir>/APPLE_ID`; `""` for an account logged in before lite recorded it) |
| `/token` | GET | returns `region`, `music_token`, `refreshed_at`, `next_refresh_at`; 503 until a music token exists |

### lite arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--login user:pass` | — | login, cache tokens, then exit |
| `--code-from-file` | off | read 2FA code from file |
| `--host` | `127.0.0.1` | listen address |
| `--port` | `12340` | listen port |
| `--base-dir` | `data` | data directory |
| `--device-info` | auto | override the entire device-info string (see [Device info](#device-info)) |
| `--proxy` | — | proxy URL |
| `--debug` | off | debug logging; disables SSL verify in Release |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log-file` | — | log file path |
| `--token-refresh-interval` | `1800` | background refresh interval in seconds; at start, the first refresh comes when `<base-dir>/MUSIC_TOKEN` (written only when a new token is obtained) reaches that age, at least 5 s in |
| `--cors` | off | send CORS headers (`Access-Control-Allow-Origin: *`) and answer `OPTIONS` preflights so browser pages on any origin can call the API; the API has no authentication, so only enable it on instances you trust to be reachable |

### Device info

`lite` identifies itself to Apple's servers with a device-info string of the
form:

```
ClientIdentifier/VersionIdentifier/PlatformIdentifier/ProductVersion/DeviceModel/BuildVersion/LocaleIdentifier/LanguageIdentifier/AndroidID
```

Example: `Music/5.0.2/Android/10/Pixel 8/7663314/en-US/en-US/<android-id>`.

The trailing `AndroidID` is derived automatically:

- During `--login`, it is the FNV-1a 64-bit hash of the account username and is
  persisted to `<base-dir>/ANDROID_ID`, so each account keeps a stable device
  identity across runs.
- In service mode (no username), the persisted `ANDROID_ID` is reused; if none
  exists, a built-in default is used.

Pass `--device-info <string>` to override the entire string explicitly.

### QEMU launcher arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--host <addr>` | `127.0.0.1` | host address the forwarded port binds to (`0.0.0.0` to expose) |
| `--host-port <port>` | `12340` | host port |
| `--guest-port <port>` | `12340` | guest port |
| `--guest-host <addr>` | `0.0.0.0` | address the guest lite listens on (must be reachable by QEMU forwarding) |
| `--memory <MB>` | `512` | guest memory in MB |
| `--smp <N>` | `2` | guest CPU count |
| `--accel <accel>` | auto | force acceleration (`kvm`, `hvf`, `whpx`, `tcg`) |
| `--qemu-bin <path>` | auto | QEMU binary path |

Environment variables (fallbacks; command-line flags take precedence):

| Variable | Default | Description |
|----------|---------|-------------|
| `LITE_QEMU_HOST` | `127.0.0.1` | host address the forwarded port binds to |
| `HOST_PORT` | `12340` | host port |
| `GUEST_PORT` | `12340` | guest port |
| `LITE_GUEST_HOST` | `0.0.0.0` | address the guest lite listens on |
| `MEMORY` | `512` | guest memory in MB |
| `SMP` | `2` | guest CPU count |
| `LITE_QEMU_ACCEL` | auto | force acceleration |
| `QEMU_BIN` | auto | QEMU binary path |



## License and credits

The whole repository is MIT ([`LICENSE`](LICENSE)).

- wrapper-lite, its launchers, `rootfs/` and `qemu/` come from
  [WorldObservationLog/wrapper](https://github.com/WorldObservationLog/wrapper)
  (MIT); `amdl` keeps upstream's history and adds its patches on top.
- `manager/` re-implements the behavior of
  [WorldObservationLog/wrapper-manager](https://github.com/WorldObservationLog/wrapper-manager)
  (AGPL-3.0). It was written from a behavior specification, without that
  project's code, and is MIT like the rest of this repository.
