#!/bin/sh
set -eu
version=${1:?usage: scripts/release.sh VERSION}
case "$version" in *[!a-zA-Z0-9._-]*|'') echo 'invalid version' >&2; exit 1;; esac
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os=${target%/*}
    arch=${target#*/}
    name=nbdproxy_${version}_${os}_${arch}
    stage=$(mktemp -d)
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/nbdproxy" .
    cp README.md LICENSE "$stage/"
    cp -R packaging "$stage/"
    tar -czf "dist/$name.tar.gz" -C "$stage" .
    rm -rf "$stage"
done
(cd dist; if command -v sha256sum >/dev/null 2>&1; then sha256sum nbdproxy_"$version"_*.tar.gz; else shasum -a 256 nbdproxy_"$version"_*.tar.gz; fi) > "dist/SHA256SUMS-$version"
