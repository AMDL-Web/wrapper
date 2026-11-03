# AMDL wrapper

Holds an Apple Music account session for [amdl](https://github.com/AMDL-Web/apple-music-downloader).
amdl asks it for playlists, decryption keys, lyrics, web-playback data, licenses
and the account's music token over HTTP (amdl's `wrapper.address`); it does the
Apple-side work amdl cannot do itself.

| Part | Directory | What it does | License |
| --- | --- | --- | --- |
| **wrapper-lite** | this root (`lite/`, launchers, `rootfs/`, `qemu/`) | Runs Apple Music's Android libraries for one logged-in account and serves the [HTTP API](#http-api) on one port. | MIT |
| **wrapper-manager** | [`manager/`](manager/) | Starts, logs in, restarts and restores up to 10 wrapper-lite instances behind one wrapper-lite-compatible port, with `POST /login` (two-step 2FA) and `POST /logout`. | AGPL-3.0 ([`manager/LICENSE`](manager/LICENSE)) |

A bare wrapper-lite can serve downloads. The manager adds the login API that
amdl's console and desktop app use to sign accounts in. Neither part has
authentication: keep its port on the machines that need it (`GET /token`
returns the account's music token). The Cloudflare deployment adds a key.

## Run it

**amdl desktop app.** Nothing to do here. The app downloads the QEMU guest
(`manager/guest/`), pinned by digest, and runs the manager inside it on this
machine.

**Docker, or a Linux host.** Run the manager, then log in from amdl's console.

```shell
git clone -b lite https://github.com/AMDL-Web/wrapper
cd wrapper/manager
docker compose up -d        # API on localhost:8080; the container is privileged
```

Without Docker (Linux x86_64, user namespaces and chroot available):

```shell
cd wrapper/manager
go build -o wrapper-manager .
./wrapper-manager -host 0.0.0.0 -port 8080
```

On first start the manager installs the wrapper-lite payload into `data/wrapper/`.
Flags, endpoints and payload pinning are in [`manager/README.md`](manager/README.md).

**Cloudflare.** One-click deploy of the manager behind a static key, with an
encrypted backup in R2 and a daily token renewal. See
[`cloudflare/README.md`](cloudflare/README.md).

Then set the address (and key, for Cloudflare) in amdl's Settings; the amdl docs
cover [the wrapper](https://github.com/AMDL-Web/apple-music-downloader/blob/dev/docs/install.md#the-wrapper)
and [its configuration](https://github.com/AMDL-Web/apple-music-downloader/blob/dev/docs/configuration.md#wrapper).

## Release assets

Every successful push build on `lite` publishes its artifacts as GitHub
pre-releases ([`publish.yml`](.github/workflows/publish.yml)); an asset is never
replaced once published, so its sha256 can be pinned:

| Tag | Asset | From |
| --- | --- | --- |
| `guest-<sha12>` | `manager-qemu-guest.zip` | `manager.yml`: the QEMU guest of the manager (`manager/guest/`) |
| `lite-<sha12>` | `wrapper-lite-linux-x86_64.zip` | `build-lite.yml`: the native payload the manager installs |

`<sha12>` is the first 12 characters of the commit. The native payload carries
22 of the 98 libraries in `rootfs/system/lib64`, the ones wrapper-lite loads;
`tools/slim-rootfs.py` removes the rest when the package is assembled, and its
`KEEP` list says why each stays. The repository's `rootfs/` stays whole.

## What differs from upstream

This is a fork of [`WorldObservationLog/wrapper@lite`](https://github.com/WorldObservationLog/wrapper/tree/lite)
with [`wrapper-manager@v2`](https://github.com/WorldObservationLog/wrapper-manager/tree/v2)
merged in under `manager/`. Upstream changes are merged in, not rebased.

wrapper-lite (`lite/`, launchers):

- **1024-key fix.** Upstream keeps the FairPlay decrypt context of every `/key`;
  after 1024 successful calls in one process every later `/key` fails with
  `KDCanProcessCKC status: -42786` until the restart. The fork frees each
  context once its template has been copied. It also fetches the persistent key
  once per `/key` instead of twice, which halves a `/key` (each fetch is a
  license round trip to Apple).
- **`GET /token`**, with `refreshed_at` and `next_refresh_at`.
- **Stale token refresh at start.** A process that starts when the cached token
  is older than `--token-refresh-interval` refreshes it a few seconds in, not a
  full interval later (a woken container, a machine back from a restart).
- **`APPLE_ID` file and `/status` `name`.** Login records the Apple ID in
  `<base-dir>/APPLE_ID`; `/status` reports it so clients can label the account.
- **Root launcher in a private mount namespace.** `wrapper-lite` run as root
  mounts in its own namespace instead of the host's.
- **Slimmed native payload** (see above) and `macos-15` in `build-lite` (GitHub
  retired `macos-14`).

wrapper-manager (`manager/`): up to 10 accounts, `/status.instances`,
`/token?instance=`, login cancel, `POST /shutdown` and durable writes for the
QEMU guest, the admin API, and a payload pinned by URL and sha256. The details
are in [`manager/README.md`](manager/README.md).

A change under `manager/`, `cloudflare/` or a `.md` file alone does not run
`build-lite`.

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


## License and credit

wrapper-lite is MIT ([`LICENSE`](LICENSE)); wrapper-manager is AGPL-3.0
([`manager/LICENSE`](manager/LICENSE)). Both come from
[WorldObservationLog](https://github.com/WorldObservationLog)'s
[wrapper](https://github.com/WorldObservationLog/wrapper) and
[wrapper-manager](https://github.com/WorldObservationLog/wrapper-manager);
this fork keeps their history.

