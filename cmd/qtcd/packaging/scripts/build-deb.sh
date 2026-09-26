#!/bin/bash
#
# build-deb.sh VERSION ARCH builds qtcd and qtc for Linux on ARCH (a Debian
# architecture: arm64, armhf, or amd64) and packages them as
# build/qtcd_VERSION_ARCH.deb and build/qtcd_VERSION_linux_ARCH.tar.gz.
# Run from the repository root. VERSION is a Debian version (0.1.0~rc1);
# the binaries report it as given.
#
# Without dpkg-deb (e.g. on macOS) it stops after staging the package tree.

set -euo pipefail

VERSION=${1:?usage: build-deb.sh VERSION ARCH}
ARCH=${2:?usage: build-deb.sh VERSION ARCH}
PKG=qtcd
SRC=cmd/qtcd/packaging

case "$ARCH" in
    arm64) GOARCH=arm64 GOARM= ;;
    armhf) GOARCH=arm GOARM=6 ;; # armv6 runs on every Raspberry Pi OS armhf board
    amd64) GOARCH=amd64 GOARM= ;;
    *) echo "unsupported ARCH $ARCH" >&2; exit 2 ;;
esac

ROOT="build/${PKG}_${VERSION}_${ARCH}"
rm -rf "$ROOT"
mkdir -p "$ROOT/DEBIAN" "$ROOT/usr/bin" "$ROOT/usr/lib/systemd/system" \
    "$ROOT/usr/share/qtcd" "$ROOT/usr/share/doc/qtcd"

LDFLAGS="-s -w -X main.version=${VERSION}"
for cmd in qtcd qtc; do
    GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH GOARM=$GOARM \
        go build -trimpath -ldflags "$LDFLAGS" -o "$ROOT/usr/bin/$cmd" "./cmd/$cmd"
done

install -m 0644 "$SRC/qtcd.service" "$ROOT/usr/lib/systemd/system/"
install -m 0644 "$SRC/qtcd.ini.sample" "$ROOT/usr/share/qtcd/"
install -m 0644 LICENSE "$ROOT/usr/share/doc/qtcd/copyright"
sed -e "s/VERSION_PLACEHOLDER/${VERSION}/" -e "s/ARCH_PLACEHOLDER/${ARCH}/" \
    "$SRC/debian/control" > "$ROOT/DEBIAN/control"
for s in postinst prerm postrm; do
    install -m 0755 "$SRC/debian/$s" "$ROOT/DEBIAN/"
done

TARBALL="build/${PKG}_${VERSION}_linux_${ARCH}.tar.gz"
tar -czf "$TARBALL" -C "$ROOT" usr
echo "Built $TARBALL"

if ! command -v dpkg-deb >/dev/null; then
    echo "dpkg-deb not found; package tree staged in $ROOT" >&2
    exit 0
fi
dpkg-deb --root-owner-group --build "$ROOT"
echo "Built ${ROOT}.deb"
