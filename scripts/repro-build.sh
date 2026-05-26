#!/usr/bin/env bash
# Build deterministic argonaut artifacts for Nitro EIF image construction.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARGONAUT_REPO_ROOT="$ROOT"
source "$ROOT/scripts/lib/repro.sh"

OUT_DIR="${ARGONAUT_REPRO_OUT_DIR:-$ROOT/dist/repro}"
BUILD_DOCKER=auto
BUILD_EIF=auto
IMAGE_TAG="${ARGONAUT_REPRO_IMAGE_TAG:-argonaut-repro:local}"

usage() {
  cat <<'EOF'
Usage: scripts/repro-build.sh [--out DIR] [--image-tag TAG] [--skip-docker] [--skip-eif]

Builds:
  argonaut-linux-amd64  deterministic static Linux binary
  rootfs.tar            normalized root filesystem tarball
  context.tar           normalized Docker build context for Nitro image builds
  manifest.json         build inputs and artifact checksums

If Docker is available, the script also builds IMAGE_TAG from context.tar.
If nitro-cli is available, it builds argonaut.eif from IMAGE_TAG.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      OUT_DIR="$2"
      shift 2
      ;;
    --image-tag)
      IMAGE_TAG="$2"
      shift 2
      ;;
    --skip-docker)
      BUILD_DOCKER=0
      BUILD_EIF=0
      shift
      ;;
    --skip-eif)
      BUILD_EIF=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

need_cmd go
need_cmd awk

argonaut_export_repro_env
argonaut_verify_go_toolchain

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR/bin" "$OUT_DIR/rootfs/etc" "$OUT_DIR/rootfs/dev" "$OUT_DIR/rootfs/tmp" "$OUT_DIR/context"
export GOCACHE="${GOCACHE:-$OUT_DIR/.gocache}"
mkdir -p "$GOCACHE"

echo "[repro] building argonaut linux/amd64 with Go $(go env GOVERSION)"
argonaut_go_build "$OUT_DIR/bin/argonaut-linux-amd64" .
install -m 0555 "$OUT_DIR/bin/argonaut-linux-amd64" "$OUT_DIR/rootfs/argonaut"
printf '127.0.0.1   localhost\n' > "$OUT_DIR/rootfs/etc/hosts"

argonaut_normalized_tar "$OUT_DIR/rootfs" "$OUT_DIR/rootfs.tar"

cp "$ROOT/build/argonaut.eif.Dockerfile" "$OUT_DIR/context/Dockerfile"
mkdir -p "$OUT_DIR/context/rootfs"
(
  cd "$OUT_DIR/rootfs"
  COPYFILE_DISABLE=1 tar -cf - .
) | (
  cd "$OUT_DIR/context/rootfs"
  tar -xf -
)
argonaut_normalized_tar "$OUT_DIR/context" "$OUT_DIR/context.tar"

if [[ "$BUILD_DOCKER" == "auto" ]]; then
  if command -v docker >/dev/null 2>&1; then
    BUILD_DOCKER=1
  else
    BUILD_DOCKER=0
  fi
fi

if [[ "$BUILD_DOCKER" == "1" ]]; then
  echo "[repro] building Docker image $IMAGE_TAG"
  docker build -q -t "$IMAGE_TAG" - < "$OUT_DIR/context.tar" >/dev/null
fi

if [[ "$BUILD_EIF" == "auto" ]]; then
  if [[ "$BUILD_DOCKER" == "1" ]] && command -v nitro-cli >/dev/null 2>&1; then
    BUILD_EIF=1
  else
    BUILD_EIF=0
  fi
fi

if [[ "$BUILD_EIF" == "1" ]]; then
  echo "[repro] building EIF"
  nitro-cli build-enclave \
    --docker-uri "$IMAGE_TAG" \
    --output-file "$OUT_DIR/argonaut.eif" > "$OUT_DIR/build-enclave.json"
fi

binary_sha="$(argonaut_sha256 "$OUT_DIR/bin/argonaut-linux-amd64")"
rootfs_sha="$(argonaut_sha256 "$OUT_DIR/rootfs.tar")"
context_sha="$(argonaut_sha256 "$OUT_DIR/context.tar")"
eif_sha=""
if [[ -f "$OUT_DIR/argonaut.eif" ]]; then
  eif_sha="$(argonaut_sha256 "$OUT_DIR/argonaut.eif")"
fi

git_commit=""
git_tree_state="unknown"
if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git_commit="$(git -C "$ROOT" rev-parse HEAD)"
  if git -C "$ROOT" diff --quiet && git -C "$ROOT" diff --cached --quiet; then
    git_tree_state="clean"
  else
    git_tree_state="dirty"
  fi
fi

cat > "$OUT_DIR/manifest.json" <<EOF
{
  "schemaVersion": "argonaut.reproducible-build.v1",
  "gitCommit": "$git_commit",
  "gitTreeState": "$git_tree_state",
  "sourceDateEpoch": $SOURCE_DATE_EPOCH,
  "goVersion": "$(go env GOVERSION)",
  "target": {
    "goos": "linux",
    "goarch": "amd64",
    "cgoEnabled": false
  },
  "goBuildFlags": [
    "-mod=readonly",
    "-trimpath",
    "-buildvcs=false",
    "-tags=netgo,osusergo",
    "-ldflags=-s -w -buildid="
  ],
  "dockerImageTag": "$IMAGE_TAG",
  "artifacts": {
    "argonaut-linux-amd64": "$binary_sha",
    "rootfs.tar": "$rootfs_sha",
    "context.tar": "$context_sha",
    "argonaut.eif": "$eif_sha"
  }
}
EOF

echo "[repro] wrote $OUT_DIR"
