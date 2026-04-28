#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

PKG="repokit-test-package-all"
IMAGE="debian:13"
ARCH="arm64"  # arch:all output is identical across build hosts

VERSIONS=(
  "1.0:1"
)

COMPRESSORS=(
  "gzip"
  "xz"
  "zstd"
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
  local upstream="$1"
  local revision="$2"
  local comp="$3"
  local outdir="$4"

  local tarball="${PKG}_${upstream}.orig.tar.gz"
  local srcdir="${PKG}-${upstream}"
  local fullver="${upstream}-${revision}"

  docker run --rm \
    --platform "linux/$ARCH" \
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

  * Test build ${fullver} (${comp}).

 -- repokit test <test@example.com>  \$(date -R)
EOF
      printf '\noverride_dh_builddeb:\n\tdh_builddeb -- -Z${comp}\n' >> debian/rules
      debuild -uc -us
      cd /tmp/work
      cp -v *.deb *.changes *.buildinfo *.dsc *.tar.* /out/
    "
}

for v in "${VERSIONS[@]}"; do
  upstream="${v%%:*}"
  revision="${v##*:}"
  fullver="${upstream}-${revision}"
  for comp in "${COMPRESSORS[@]}"; do
    outdir="$OUT/$fullver/$comp"
    mkdir -p "$outdir"

    echo
    echo "=== building ${PKG} $fullver $comp -> $outdir ==="
    if build_one "$upstream" "$revision" "$comp" "$outdir"; then
      echo "=== $fullver $comp: OK ==="
    else
      echo "=== $fullver $comp: FAILED ===" >&2
      failed+=("$fullver/$comp")
    fi
  done
done

echo
echo "=== summary ==="
for v in "${VERSIONS[@]}"; do
  upstream="${v%%:*}"
  revision="${v##*:}"
  fullver="${upstream}-${revision}"
  for comp in "${COMPRESSORS[@]}"; do
    key="$fullver/$comp"
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
