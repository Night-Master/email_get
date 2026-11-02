#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
WEB_DIR="$ROOT_DIR/web"
OUT_DIR="$ROOT_DIR/release"
GOPROXY_VALUE="${GOPROXY:-https://goproxy.cn,direct}"
TARGETS="${TARGETS:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64}"

echo "[1/3] Build frontend"
cd "$WEB_DIR"
npm install
npm run build

echo "[2/3] Build Go binaries"
cd "$ROOT_DIR"
mkdir -p "$OUT_DIR"

build_target() {
  local target="$1"
  local goos="${target%/*}"
  local goarch="${target#*/}"
  local ext=""
  if [[ "$goos" == "windows" ]]; then
    ext=".exe"
  fi
  local output="$OUT_DIR/email_get_${goos}_${goarch}${ext}"
  echo "  -> [$goos/$goarch] start $(date '+%H:%M:%S')"
  GOPROXY="$GOPROXY_VALUE" GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    /usr/bin/time -p go build -o "$output" .
  echo "  -> [$goos/$goarch] done  $(date '+%H:%M:%S')"
}

for target in $TARGETS; do
  build_target "$target"
done

echo "[3/3] Done"
ls -lh "$OUT_DIR"
