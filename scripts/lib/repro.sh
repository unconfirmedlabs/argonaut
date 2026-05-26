#!/usr/bin/env bash

ARGONAUT_REPO_ROOT="${ARGONAUT_REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

argonaut_expected_go_version() {
  tr -d '[:space:]' < "$ARGONAUT_REPO_ROOT/.go-version"
}

argonaut_source_date_epoch() {
  if [[ -n "${SOURCE_DATE_EPOCH:-}" ]]; then
    [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || {
      echo "SOURCE_DATE_EPOCH must be a Unix timestamp" >&2
      return 1
    }
    printf '%s\n' "$SOURCE_DATE_EPOCH"
    return 0
  fi

  if git -C "$ARGONAUT_REPO_ROOT" rev-parse --verify HEAD >/dev/null 2>&1; then
    git -C "$ARGONAUT_REPO_ROOT" log -1 --format=%ct
    return 0
  fi

  printf '0\n'
}

argonaut_export_repro_env() {
  export SOURCE_DATE_EPOCH
  SOURCE_DATE_EPOCH="$(argonaut_source_date_epoch)"
  export TZ=UTC
  export LC_ALL=C
  export LANG=C
  umask 022
}

argonaut_verify_go_toolchain() {
  local expected actual
  expected="$(argonaut_expected_go_version)"
  actual="$(go env GOVERSION)"
  actual="${actual#go}"

  if [[ "$actual" != "$expected" && -z "${ARGONAUT_ALLOW_HOST_GO:-}" ]]; then
    cat >&2 <<EOF
argonaut reproducible builds require Go $expected; found Go $actual.
Install the pinned toolchain from .go-version or set ARGONAUT_ALLOW_HOST_GO=1 for non-release local checks.
EOF
    return 1
  fi
}

argonaut_go_build() {
  local out="$1"
  local pkg="$2"

  (
    cd "$ARGONAUT_REPO_ROOT"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -mod=readonly \
      -trimpath \
      -buildvcs=false \
      -tags=netgo,osusergo \
      -ldflags='-s -w -buildid=' \
      -o "$out" \
      "$pkg"
  )
  chmod 0555 "$out"
}

argonaut_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

argonaut_gnu_tar() {
  if command -v gtar >/dev/null 2>&1; then
    printf 'gtar\n'
    return 0
  fi
  if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    printf 'tar\n'
    return 0
  fi

  echo "GNU tar is required for normalized archive output; install gtar or run on Linux." >&2
  return 1
}

argonaut_normalized_tar() {
  local src="$1"
  local out="$2"
  local tar_bin

  tar_bin="$(argonaut_gnu_tar)"
  COPYFILE_DISABLE=1 "$tar_bin" \
    --sort=name \
    --mtime="@$SOURCE_DATE_EPOCH" \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    --mode='u+rwX,go+rX,go-w' \
    -C "$src" \
    -cf "$out" \
    .
}
