#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

PACKAGES=(
  "repokit-test-package"
  "repokit-test-package-all"
  "repokit-test-multibin"
)

failed=()

for pkg in "${PACKAGES[@]}"; do
  echo
  echo "########## $pkg ##########"
  if ( cd "$pkg" && ./build.sh ); then
    echo "########## $pkg: OK ##########"
  else
    echo "########## $pkg: FAILED ##########" >&2
    failed+=("$pkg")
  fi
done

echo
echo "=== top-level summary ==="
for pkg in "${PACKAGES[@]}"; do
  if [[ " ${failed[*]-} " == *" $pkg "* ]]; then
    echo "  $pkg: FAIL"
  else
    echo "  $pkg: ok"
  fi
done

if ((${#failed[@]} > 0)); then
  exit 1
fi
