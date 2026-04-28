#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

PKG="repokit-test-multibin"
IMAGE="debian:13"

# upstream version : debian revision : epoch (empty for none)
VERSIONS=(
  "1.0:1:1"
)

ARCHES=(
  "arm64"
  "amd64"
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
  local arch="$1"
  local upstream="$2"
  local revision="$3"
  local epoch="$4"
  local comp="$5"
  local outdir="$6"

  local tarball="${PKG}_${upstream}.orig.tar.gz"
  local srcdir="${PKG}-${upstream}"
  local fullver
  if [[ -n "$epoch" ]]; then
    fullver="${epoch}:${upstream}-${revision}"
  else
    fullver="${upstream}-${revision}"
  fi

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
  rest="${v#*:}"
  revision="${rest%%:*}"
  epoch="${rest#*:}"
  if [[ "$epoch" == "$rest" ]]; then epoch=""; fi
  if [[ -n "$epoch" ]]; then
    label="${epoch}_${upstream}-${revision}"
  else
    label="${upstream}-${revision}"
  fi
  for arch in "${ARCHES[@]}"; do
    for comp in "${COMPRESSORS[@]}"; do
      outdir="$OUT/$label/$arch/$comp"
      mkdir -p "$outdir"

      echo
      echo "=== building ${PKG} $label $arch $comp -> $outdir ==="
      if build_one "$arch" "$upstream" "$revision" "$epoch" "$comp" "$outdir"; then
        echo "=== $label $arch $comp: OK ==="
      else
        echo "=== $label $arch $comp: FAILED ===" >&2
        failed+=("$label/$arch/$comp")
      fi
    done
  done
done

echo
echo "=== summary ==="
for v in "${VERSIONS[@]}"; do
  upstream="${v%%:*}"
  rest="${v#*:}"
  revision="${rest%%:*}"
  epoch="${rest#*:}"
  if [[ "$epoch" == "$rest" ]]; then epoch=""; fi
  if [[ -n "$epoch" ]]; then
    label="${epoch}_${upstream}-${revision}"
  else
    label="${upstream}-${revision}"
  fi
  for arch in "${ARCHES[@]}"; do
    for comp in "${COMPRESSORS[@]}"; do
      key="$label/$arch/$comp"
      if [[ " ${failed[*]-} " == *" $key "* ]]; then
        echo "  $key: FAIL"
      else
        echo "  $key: ok ($OUT/$key)"
      fi
    done
  done
done

if ((${#failed[@]} > 0)); then
  exit 1
fi
