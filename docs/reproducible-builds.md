# Reproducible Builds

Argonaut release artifacts are built from pinned inputs:

- `.go-version` pins the release Go toolchain.
- `go.sum` pins module contents.
- `scripts/lib/repro.sh` centralizes release build flags.
- `scripts/repro-build.sh` creates normalized binary, rootfs, and Docker context
  artifacts for Nitro EIF construction.
- CI builds the reproducible artifacts twice and compares the bytes.

The reproducibility contract assumes the same source tree, same `.go-version`,
same `go.sum`, same `SOURCE_DATE_EPOCH`, and same Docker and `nitro-cli`
versions when producing an EIF.

## Build

For a local non-EIF check:

```bash
scripts/repro-build.sh --skip-docker
```

The script requires GNU tar for normalized archive metadata. Linux CI runners
already have it; on macOS install `gtar`.

For a Nitro host with Docker and `nitro-cli` installed:

```bash
scripts/repro-build.sh --image-tag argonaut-repro:$(git rev-parse --short HEAD)
```

The script writes:

- `dist/repro/bin/argonaut-linux-amd64`
- `dist/repro/rootfs.tar`
- `dist/repro/context.tar`
- `dist/repro/manifest.json`
- `dist/repro/argonaut.eif` when `nitro-cli` is available
- `dist/repro/build-enclave.json` when `nitro-cli` is available

`manifest.json` records the commit, tree state, Go version, target, build flags,
image tag, and artifact hashes. `build-enclave.json` records the Nitro
measurements emitted by `nitro-cli`.

## Controls

The Go binary is built with:

```text
CGO_ENABLED=0 GOOS=linux GOARCH=amd64
-mod=readonly -trimpath -buildvcs=false -tags=netgo,osusergo
-ldflags='-s -w -buildid='
```

Those flags avoid host C libraries, absolute source paths, implicit VCS stamping,
and variable linker build IDs.

`SOURCE_DATE_EPOCH` defaults to the commit timestamp for `HEAD`. Set it
explicitly when building from an exported source tree without `.git` metadata:

```bash
SOURCE_DATE_EPOCH=0 scripts/repro-build.sh
```

Release builds require the exact Go version in `.go-version`. For a local smoke
check with a different Go patch version, set:

```bash
ARGONAUT_ALLOW_HOST_GO=1 scripts/repro-build.sh --skip-docker
```

Do not publish artifacts produced with `ARGONAUT_ALLOW_HOST_GO=1`.

## Nitro EIF Notes

An EIF is reproducible only across the same Docker and `nitro-cli` behavior.
Keep the generated `manifest.json` and `build-enclave.json` with release
artifacts, and compare both the EIF hash and Nitro PCR measurements when
rebuilding.

The smoke and benchmark scripts use the same deterministic Go build helper. Set
`ARGONAUT_REQUIRE_PINNED_GO=1` when those scripts are run as release gates.
