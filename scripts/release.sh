#!/bin/sh
set -eu
version=${1:?usage: scripts/release.sh VERSION}
case "$version" in *[!a-zA-Z0-9._-]*|'') echo 'invalid version' >&2; exit 1;; esac
command -v nfpm >/dev/null 2>&1 || { echo "nfpm is required (go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.0)" >&2; exit 1; }
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os=${target%/*}
    arch=${target#*/}
    name=nbdproxy_${version}_${os}_${arch}
    stage=$(mktemp -d)
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/nbdproxy" .
    cp README*.md LICENSE "$stage/"
    cp -R packaging "$stage/"
    tar -czf "dist/$name.tar.gz" -C "$stage" .
    if [ "$os" = linux ]; then
        sed 's|/usr/local/bin/nbdproxy|/usr/bin/nbdproxy|' packaging/nbdproxy.service > "$stage/nbdproxy.service"
        sed "s|@PACKAGE_STAGE@|$stage|g" packaging/nfpm.yaml > "$stage/nfpm.yaml"
        for format in deb rpm; do
            PACKAGE_VERSION="${version#v}" PACKAGE_ARCH="$arch" \
                nfpm package --config "$stage/nfpm.yaml" --packager "$format" --target "dist/$name.$format"
        done
    fi
    rm -rf "$stage"
done
(cd dist; if command -v sha256sum >/dev/null 2>&1; then sha256sum nbdproxy_"$version"_*.tar.gz nbdproxy_"$version"_*.deb nbdproxy_"$version"_*.rpm; else shasum -a 256 nbdproxy_"$version"_*.tar.gz nbdproxy_"$version"_*.deb nbdproxy_"$version"_*.rpm; fi) > "dist/SHA256SUMS-$version"
