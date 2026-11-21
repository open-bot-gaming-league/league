#!/bin/sh
# entrypoint.sh: starts the Go table server on 127.0.0.1:9790 (never
# exposed outside the container), waits for its /health, then execs
# the MCP server in the foreground (Streamable HTTP mode, bound to
# $PORT -- Cloud Run sets this; defaults to 8080 for a local container
# run).
#
# Session state ("local disk", v0 -- see CLOUDRUN.md): the table
# server restores sessions from a hands file on boot and appends to it
# as hands finish. That file lives under $OBGL_STATE_DIR (default
# /state), deliberately NOT under ./table/data/: the bot data (bucket
# files, blueprints, served.json) is baked into the image there, and
# if that directory is ever replaced wholesale by a Cloud Run volume
# mount (see CLOUDRUN.md's option (a2)), hands.jsonl must not be a
# file inside that same mount unless the mount is meant to hold it --
# keeping them on separate paths means a bot-data mount and a
# session-state mount (or no mount at all) can be decided
# independently, not coupled by an accident of directory layout.
# Inside a plain container run $OBGL_STATE_DIR is genuinely local and
# lost on the next `docker run`; on Cloud Run its persistence depends
# on whether that path is backed by a mounted volume -- see
# CLOUDRUN.md's comparison before relying on it.
set -eu

TABLE_ADDR=127.0.0.1:9790
STATE_DIR="${OBGL_STATE_DIR:-/state}"
mkdir -p "$STATE_DIR"

./table/bin/table \
  -config ./table/configs/table_50bb.json \
  -bots ./table/data/served.json \
  -hands "$STATE_DIR/hands.jsonl" \
  -addr "$TABLE_ADDR" &
TABLE_PID=$!

cleanup() {
  kill "$TABLE_PID" 2>/dev/null || true
}
trap cleanup TERM INT

# The table has no readiness signal but its own /health; poll it rather than
# guessing a fixed sleep -- startup time depends on how fast the
# ~850 MB of bucket files mmap fault in. python, not curl/wget: the
# python:3.12-slim base has no HTTP client binary installed, and this
# avoids an extra apt layer for one health check.
health_check() {
  python -c "
import sys, urllib.request
try:
    urllib.request.urlopen('http://$TABLE_ADDR/health', timeout=1)
except Exception:
    sys.exit(1)
"
}

i=0
while ! health_check; do
  i=$((i + 1))
  if [ "$i" -ge 150 ]; then
    echo "entrypoint: table server did not answer /health within 30s" >&2
    cleanup
    exit 1
  fi
  if ! kill -0 "$TABLE_PID" 2>/dev/null; then
    echo "entrypoint: table server exited during startup" >&2
    exit 1
  fi
  sleep 0.2
done

export OBGL_TABLE_URL="http://$TABLE_ADDR/"
export OBGL_TRANSPORT=http
exec python ./mcp/server.py
