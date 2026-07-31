# Building amp images locally on Apple Silicon (Rosetta)

The amp images are `linux/amd64`. On an Apple Silicon (arm64) Mac, building them
means running amd64 binaries under emulation — and **the default emulator (qemu)
crashes the compile**: the endpoint image's Rust stage dies with
`qemu: uncaught target signal 11 (SIGSEGV)` while running `rustc` (it leaves
`qemu_rustc_*.core` dumps). The controller image's Go/Rust build hits the same wall.

The fix is to make the podman machine run amd64 binaries under **Rosetta** instead
of qemu. Rosetta is Apple's binary translator — far more robust and faster than
qemu — and it emits genuine `x86_64` output, so the resulting images run correctly
on real amd64 nodes. With it enabled, `deploy/endpoint/Dockerfile` builds locally
in ~24 min.

## Why qemu is used by default (and Rosetta isn't)

`podman machine` (applehv) ships with Rosetta support, but activation is gated on a
file that isn't created by default. Its `rosetta-activation.service` is skipped
every boot on `ConditionPathExists=/etc/containers/enable-rosetta`, so the
`rosetta` binfmt handler is never registered and `qemu-x86_64` stays the handler
for amd64 ELF binaries. `podman machine inspect` may report `Rosetta: true` even
though Rosetta is not actually wired into execution.

## Enable Rosetta

```sh
# one-time: flip the activation gate inside the machine (persists across reboots)
podman machine ssh podman-machine-default \
  'sudo touch /etc/containers/enable-rosetta && sudo systemctl start rosetta-activation.service'

# verify: 'rosetta' is now the amd64 handler and qemu-x86_64 is gone
podman machine ssh podman-machine-default \
  'ls /proc/sys/fs/binfmt_misc/ | grep -E "rosetta|qemu-x86_64"'   # -> rosetta
```

The activation script mounts the Rosetta virtiofs share, registers the `rosetta`
binfmt handler (with the `F` flag so it works inside build containers), and
unregisters `qemu-x86_64`.

## Give the machine enough RAM

Linking the `pageserver` crate needs headroom; the 8 GB default can OOM. Bump it
(this restarts the machine — Rosetta re-activates automatically on boot):

```sh
podman machine set --memory 16384   # 16 GB; requires a stop/start
```

## Build

```sh
# endpoint image (Postgres 18 + the full Rust compute stack); ~24 min
podman build --platform linux/amd64 -f deploy/endpoint/Dockerfile \
  --build-arg GIT_VERSION="$(git rev-parse HEAD)" \
  --build-arg BUILD_TAG=<tag> \
  -t <registry>/amp/endpoint:<tag> .
```

Notes:
- Requires the `compute/vendor/postgres-v18` submodule checked out
  (`git submodule update --init --recursive compute/vendor/postgres-v18`).
- The `build-tools` base image contains files owned by a high UID (~197609); the
  podman machine's `core` user has a subuid range of `100000:1000000`, which covers
  it, so the rootless build extracts fine. (On a host with a smaller subuid range,
  that extraction fails — build with rootful podman there instead.)
- No arm64 shortcut: the meko-dev nodes are x86-only, so a native arm64 image won't
  run there. Rosetta gives a genuine amd64 image without leaving the Mac.

## Alternative

A native x86_64 Linux box builds these with no emulation at all — plain
`podman build --platform linux/amd64 …`. Use that if Rosetta is unavailable.
