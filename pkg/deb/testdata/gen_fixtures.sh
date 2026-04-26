#!/bin/sh
# gen_fixtures.sh regenerates the .deb test fixtures used by pkg/deb.
#
# It runs the pure-Go generator gen.go so the script works on any platform
# that has Go installed (no dpkg-deb / xz / zstd dependency).
set -eu
cd "$(dirname "$0")"
go run gen.go
