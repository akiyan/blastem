#!/usr/bin/env bash
# Build and regression-test the akiyan additions: core, MCP server, and fixture.
# Usage: tools/akiyan-check.sh [OUTPUT_DIR]
# Requires: SDL2/GLEW/GL development files, Go, m68k-linux-gnu binutils,
# xvfb-run, and ffmpeg/ffprobe. Logs and artifacts go to OUTPUT_DIR.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-"$ROOT/.akiyan-check"}
JOBS=${JOBS:-2}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
# Keep the user's BlastEM configuration and saves out of the tests.
mkdir -p "$OUT/config" "$OUT/data"
export XDG_CONFIG_HOME="$OUT/config" XDG_DATA_HOME="$OUT/data"
export BLASTEM_SHOWCASE_DIR="$OUT/fixture"
# Unix-domain socket paths are limited to ~108 bytes; keep test temp dirs short.
TMPDIR=$(mktemp -d /tmp/akiyan-check.XXXXXX)
export TMPDIR
trap 'rm -rf -- "${TMPDIR:?}"' EXIT

step() {
	local name=$1
	shift
	echo "== $name: $OUT/$name.log"
	if ! "$@" >"$OUT/$name.log" 2>&1; then
		tail -n 40 "$OUT/$name.log" >&2
		echo "FAILED: $name" >&2
		exit 1
	fi
}

step core-build make -C "$ROOT" -j"$JOBS" DEBUG=1
step mcp-check make -C "$ROOT/mcp" check build fixture
step core-tests xvfb-run -a python3 -m unittest discover -s "$ROOT/tests" -v
step mcp-integration xvfb-run -a make -C "$ROOT/mcp" integration-fixture
echo "passed"
