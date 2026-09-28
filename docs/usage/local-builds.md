---
type: Guide
title: Local builds
description: How coprctl builds RPMs and SRPMs locally, the backends, and when to use each.
status: stable
---

# Local builds

Copr builds on its servers, but you do not need a Linux host or a Copr queue to
iterate on a spec. `coprctl` builds the same source RPM and, for preflight, the
same package locally so you catch problems before they reach the shared
infrastructure.

Two commands do local work:

- `coprctl build srpm` produces a source RPM from a spec.
- `coprctl try` runs the full preflight: build the SRPM, then rebuild it in a
  clean buildroot, mirroring what Copr does.

Both take a `--runtime` flag that picks the backend.

## Backends

`--runtime` accepts `auto` (default), `container`, `native`, and `mock`.

| Backend | What runs | Fidelity | Requires |
|---|---|---|---|
| `container` | the `rpmbuilder` image via podman or docker | high (clean buildroot) | a container runtime |
| `mock` | `mock --buildsrpm` / `mock --rebuild` | high (clean buildroot) | `mock` + the `mock` group |
| `native` | `spectool` + `rpmbuild` on the host | low (host buildroot) | `rpmbuild` and `spectool` |

`auto` prefers the container when one is available, then mock for the preflight
intent (a clean buildroot), then native. For the SRPM intent, `auto` falls back
directly to native, because an SRPM build does not need a clean buildroot.

## The container backend

The container backend runs the `quay.io/abn/rpmbuilder` image. The image bakes
in two environment conventions:

- `SOURCES=/sources` - where the image expects the spec and source files.
- `OUTPUT=/output` - where it writes produced RPMs.

`coprctl` mounts your spec directory at `/sources` and overrides
`OUTPUT=/sources/.rpmbuild` so the produced RPM lands back in your spec
directory under `.rpmbuild/`. A preflight runs the image twice, so the
rebuild stage also overrides `SOURCES=/sources/.rpmbuild` to read the SRPM
the source stage wrote. The script inside the image
(`/usr/bin/rpmbuilder`) then:

1. Installs the spec's `BuildRequires` with `dnf builddep`.
2. Fetches the spec's `SourceN` entries with `spectool --get-files`.
3. Builds the SRPM with `rpmbuild -bs`, or the full build with `rpmbuild -ba`.

Because step 2 fetches remote sources, a spec that references
`Source0: https://...` works without you vendoring the tarball.

The chroot name maps to an image tag: `fedora-44-x86_64` uses
`rpmbuilder:fedora-44`, `epel-9-x86_64` uses a Rocky 9 substitute, and a chroot
with no image is reported uncovered. The container never writes into your
repository; output goes to `.rpmbuild/`.

## The native backend

`native` runs `spectool --get-files` then `rpmbuild -bs` (or `-ba` for a
preflight) on the host. It is the fallback when no container is present and the
simplest option for producing an SRPM. It is not a clean buildroot: the build
runs against your host's installed packages, so `try` warns that mock is the
higher-fidelity choice.

## The mock backend

`mock` builds inside a clean buildroot that closely mirrors Copr's, which is
why it is the preferred fallback for a preflight when no container is
available. It needs the `mock` package and your user in the `mock` group:

```bash
sudo dnf install mock
sudo usermod -aG mock $USER
# re-login, then verify with: groups | grep mock
```

When mock is requested but not set up, `coprctl` reports these instructions
instead of failing silently.

## Shell completion

The `--chroot` flag completes chroot names from the cached catalog, so
`coprctl build srpm --chroot fedo<TAB>` lists matching chroots without a
network round trip. Project references complete from the API: type the owner
prefix and `<TAB>` to see your projects. Generate the completion script once:

```bash
coprctl completion bash > /etc/bash_completion.d/coprctl   # or zsh, fish
```