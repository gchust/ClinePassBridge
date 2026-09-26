#!/usr/bin/env bash
set -euo pipefail

version="${1:?version is required}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-fork\.[1-9][0-9]*)?$ ]]
case "${GOARCH:?GOARCH is required}" in
  amd64) machine=x86_64 ;;
  arm64) machine=arm64 ;;
  *) echo 'Only macOS amd64 and arm64 are supported' >&2; exit 1 ;;
esac
export CGO_ENABLED=1 GOOS=darwin
python3 .github/scripts/fork_release.py check
go test ./...
mkdir -p dist
go build -trimpath -buildvcs=false -buildmode=c-shared -ldflags='-s -w' \
  -o dist/clinepassbridge.dylib ./cmd/passbridge
test "$(lipo -archs dist/clinepassbridge.dylib)" = "$machine"
archive="dist/clinepassbridge_${version}_darwin_${GOARCH}.zip"
zip -j "$archive" dist/clinepassbridge.dylib
test "$(unzip -Z1 "$archive")" = clinepassbridge.dylib
cp marketplace/registry.json dist/registry.json
