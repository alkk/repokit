#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

PKG="repokit-test-package"
IMAGE="debian:13"

# upstream version : debian revision
VERSIONS=(
  "1.0:1"
  "1.0:2"
  "1.1:1"
)

ARCHES=(
  "arm64"
  "amd64"
)

OUT="out"
rm -rf "$OUT"
mkdir -p "$OUT"

for v in "${VERSIONS[@]}"; do
  upstream="${v%%:*}"
  if [[ ! -f "${PKG}_${upstream}.orig.tar.gz" ]]; then
    echo "missing ${PKG}_${upstream}.orig.tar.gz in $PWD" >&2
    exit 1
  fi
done

failed=()

build_one() {
  local arch="$1"
  local upstream="$2"
  local revision="$3"
  local outdir="$4"

  local tarball="${PKG}_${upstream}.orig.tar.gz"
  local srcdir="${PKG}-${upstream}"
  local fullver="${upstream}-${revision}"

  docker run --rm \
    --platform "linux/$arch" \
    -e DEBIAN_FRONTEND=noninteractive \
    -v "$PWD/$tarball:/build/$tarball:ro" \
    -v "$PWD/$outdir:/out" \
    "$IMAGE" bash -euo pipefail -c "
      apt-get update -qq
      apt-get install -y --no-install-recommends debhelper-compat devscripts build-essential
      mkdir -p /tmp/work
      cd /tmp/work
      cp /build/$tarball .
      tar xzf $tarball
      cd $srcdir
      cat > debian/changelog <<EOF
${PKG} (${fullver}) unstable; urgency=medium

  * Test build ${fullver}.

 -- repokit test <test@example.com>  \$(date -R)
EOF
      debuild -uc -us
      cd /tmp/work
      cp -v *.deb *.changes *.buildinfo *.dsc *.tar.* /out/
    "
}

for v in "${VERSIONS[@]}"; do
  upstream="${v%%:*}"
  revision="${v##*:}"
  fullver="${upstream}-${revision}"
  for arch in "${ARCHES[@]}"; do
    outdir="$OUT/$fullver/$arch"
    mkdir -p "$outdir"

    echo
    echo "=== building ${PKG} $fullver $arch -> $outdir ==="
    if build_one "$arch" "$upstream" "$revision" "$outdir"; then
      echo "=== $fullver $arch: OK ==="
    else
      echo "=== $fullver $arch: FAILED ===" >&2
      failed+=("$fullver/$arch")
    fi
  done
done

echo
echo "=== summary ==="
for v in "${VERSIONS[@]}"; do
  upstream="${v%%:*}"
  revision="${v##*:}"
  fullver="${upstream}-${revision}"
  for arch in "${ARCHES[@]}"; do
    key="$fullver/$arch"
    if [[ " ${failed[*]-} " == *" $key "* ]]; then
      echo "  $key: FAIL"
    else
      echo "  $key: ok ($OUT/$key)"
    fi
  done
done

if ((${#failed[@]} > 0)); then
  exit 1
fi
