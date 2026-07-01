# wrapper

A tool to decrypt Apple Music songs. An active subscription is still needed.

Supports only x86_64 and arm64 Linux.

## Installation

Installation methods:

- [Docker](#docker) (recommended)
- Prebuilt binaries (from [releases](https://github.com/WorldObservationLog/wrapper/releases) or [actions](https://github.com/WorldObservationLog/wrapper/actions))
- [Build from source](#build-from-source)

### Docker

Available for x86_64 and arm64. Need to download prebuilt version from releases or actions.

1. Build image:

```
docker build --tag ghcr.io/worldobservationlog/wrapper:local .
```

2. Login:

```
docker run --privileged --rm -it -v ./rootfs/data:/app/rootfs/data --entrypoint ./wrapper ghcr.io/worldobservationlog/wrapper:local -L "username:password" -H 0.0.0.0
```

Quit after this (using Ctrl-C).

3. Run:

```
docker run --privileged -v ./rootfs/data:/app/rootfs/data -p 10020:10020 -p 20020:20020 -p 30020:30020 -e args="-H 0.0.0.0" ghcr.io/worldobservationlog/wrapper:local
```


### Build from source

1. Install dependencies:

- Build tools:

  ```
  sudo apt install build-essential cmake curl unzip git
  ```

- LLVM:

  ```
  sudo bash -c "$(wget -O - https://apt.llvm.org/llvm.sh)"
  ```

- Android NDK r23b:
  ```
  curl -fLO https://dl.google.com/android/repository/android-ndk-r23b-linux.zip
  unzip -d . android-ndk-r23b-linux.zip
  ```

2. Build:

```
git clone https://github.com/WorldObservationLog/wrapper
cd wrapper
mkdir build
cd build
cmake ..
make -j$(nproc)
```

## Usage

```
Usage: wrapper [OPTION]...

  -h, --help              Print help and exit
  -V, --version           Print version and exit
  -H, --host=STRING         (default=`127.0.0.1')
  -D, --decrypt-port=INT    (default=`10020')
  -M, --m3u8-port=INT       (default=`20020')
  -A, --account-port=INT    (default=`30020')
  -P, --proxy=STRING        (default=`')
  -L, --login=STRING        (username:password)
  -F, --code-from-file      (default=off)
```

## Special thanks

- Anonymous, for providing the original version of this project and the legacy Frida decryption method.
- chocomint, for providing support for arm64 arch.

## AMDL-Web Fork Enhancements

This repository contains local enhancements to improve concurrency, resilience, and automated build flows:

- **Multi-threaded Connection Handling**: Spawns independent POSIX threads (`pthread`) to handle `decrypt`, `m3u8`, and `account` network connections. This avoids blocking synchronous handling and allows concurrent decryption processing.
- **Resilient Daemon Mode**: In concurrent mode, callback triggers like lease ending (`endLeaseCb`) or playback errors (`pbErrCb`) are intercepted and logged without terminating the wrapper process (`exit`). This prevents single-request failures from shutting down the entire service.
- **Increased Listen Backlog**: The listen socket backlog is raised from `5` to `32` to accommodate higher concurrent decryption requests.
- **GitHub Actions Workflows**:
  - **Build for x86_64**: Automatically compiles the wrapper binary on push/PR, updates the `wrapper.x86_64.latest` tag, and uploads the latest zip artifact.
  - **wrapper-qemu**: Post-build workflow that downloads a basic QEMU disk image, mounts it using `qemu-nbd`, copies the newly compiled `wrapper` binary into the image, and packages it as an artifact.
