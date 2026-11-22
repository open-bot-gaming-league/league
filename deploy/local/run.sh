#!/bin/bash
# run.sh <sha>: run the league repo at <sha> as one process tree on this
# box, the way the Cloud Run container runs it: the Go table on a private
# loopback port, the MCP server in Streamable HTTP mode in front of it.
# Local only. Read README.md in this directory for every place it differs
# from the image. Stop it with SIGTERM/SIGINT (or `bg kill`); the whole
# process group goes down.
#
#   run.sh <sha-or-ref>
#
# Environment (all optional):
#   (required: OBGL_LOCAL_ROOT, and OBGL_BOT_SRC for stage-data.sh: see common.sh)
#   OBGL_LOCAL_PORT        MCP server port (default 9792; $PORT inside the container)
#   OBGL_LOCAL_TABLE_PORT  the table's loopback port (default 9793; 9790 in the image)
#   OBGL_LOCAL_ROOT        (required) where trees, venvs, data and state live
#   OBGL_REPO              the git repo to take <sha> from (default: the one this
#                          script is in); `git fetch origin` (anonymous read) if the
#                          sha is not there
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
need OBGL_LOCAL_ROOT
ROOT=$OBGL_LOCAL_ROOT
REPO=${OBGL_REPO:-$(git -C "$HERE" rev-parse --show-toplevel)}
PORT=${OBGL_LOCAL_PORT:-9792}
TABLE_PORT=${OBGL_LOCAL_TABLE_PORT:-9793}

die() { echo "run.sh: $*" >&2; exit 1; }
[ $# -eq 1 ] || die "usage: run.sh <sha-or-ref>"

# --- the sha ---------------------------------------------------------------
resolve() { git -C "$REPO" rev-parse --verify --quiet "$1^{commit}"; }
SHA=$(resolve "$1" || true)
if [ -z "$SHA" ]; then
  echo "run.sh: $1 not in $REPO; git fetch origin (anonymous, read-only)" >&2
  git -C "$REPO" fetch -q origin
  SHA=$(resolve "$1" || true)
fi
[ -n "$SHA" ] || die "cannot resolve $1 in $REPO (a sha from another worktree: git fetch <that worktree> <branch> first)"

# --- ports: refuse to share, never take over -----------------------------------
for p in "$PORT" "$TABLE_PORT"; do
  python3 -c "
import socket, sys
s = socket.socket(); s.settimeout(0.5)
sys.exit(0 if s.connect_ex(('127.0.0.1', $p)) != 0 else 1)" || die "port $p is already in use; set OBGL_LOCAL_PORT / OBGL_LOCAL_TABLE_PORT"
done

# --- bot data (verified against the manifest every run) --------------------
"$HERE/stage-data.sh"
DATA=${OBGL_LOCAL_DATA:-$ROOT/data}

# --- the image tree for this sha (built once; the sha fixes its content) ---
TREE=$ROOT/trees/$SHA
if [ ! -e "$TREE/.built" ]; then
  [ ! -e "$TREE" ] || die "$TREE is a half-made tree (no .built marker): remove it and rerun"
  mkdir -p "$TREE/context"
  git -C "$REPO" archive "$SHA" | tar -x -C "$TREE/context"
  CTX=$TREE/context
  # Stage the bot data where the Dockerfile's `COPY deploy/cloudrun/data/` looks
  # (data/README.md: gitignored, staged before build).
  cp -P "$DATA"/* "$CTX/deploy/cloudrun/data/"
  # Go: the image builds with golang:1.27; this uses the go on PATH.
  command -v go >/dev/null || die "no go on PATH"
  python3 "$HERE/image.py" --context "$CTX" --out "$TREE/fs" --venvs "$ROOT/venvs" \
      --build-arg "OBGL_GIT_SHA=$SHA"
  # The one edit to entrypoint.sh: its table address is hard-coded to the
  # port the box's other table already holds. Must match exactly once.
  EP=$(ls "$TREE"/fs/*/app/entrypoint.sh)
  [ "$(grep -c '^TABLE_ADDR=127.0.0.1:9790$' "$EP")" = 1 ] || die "entrypoint.sh no longer has the TABLE_ADDR line run.sh patches"
  sed -i "s|^TABLE_ADDR=127.0.0.1:9790\$|TABLE_ADDR=127.0.0.1:$TABLE_PORT|" "$EP"
  touch "$TREE/.built"
fi
# A bare `cmp` is enough: served.json is a copy of this sha's example when staged
# from the same sha; a different example is worth a loud line, not a failure.
cmp -s "$DATA/served.json" "$TREE/context/deploy/cloudrun/served.example.json" \
  || echo "run.sh: WARNING: $SHA's served.example.json differs from the staged served.json" >&2

# --- run: the image's ENV, then the entrypoint, with the local overrides ---------
# shellcheck disable=SC1091
. "$TREE/fs/image.env"
STATE=$ROOT/state/$SHA-$(date -u +%Y%m%dT%H%M%SZ)   # fresh per run, like a new instance
mkdir -p "$STATE"
export PORT HOST=127.0.0.1 OBGL_STATE_DIR=$STATE
# OAuth's issuer is the URL clients reach us at: here, not the image's public
# origin (a sha without OAuth ignores it). The image's file store lands in
# $STATE, so tokens last one run, like a new instance's.
export OBGL_PUBLIC_URL=http://127.0.0.1:$PORT
# The image only has OBGL_GIT_SHA if its Dockerfile has the ARG (the /version commit).
echo "run.sh: $SHA on 127.0.0.1:$PORT (table 127.0.0.1:$TABLE_PORT, state $STATE)"
cd "$IMAGE_WORKDIR"
set -m   # the entrypoint execs python and leaves its table child behind: own group, killed as one
"${IMAGE_ENTRYPOINT[@]}" &
CHILD=$!
trap 'kill -TERM -- -$CHILD 2>/dev/null || true' TERM INT
rc=0
wait "$CHILD" || rc=$?
# the MCP server is gone; take the table with it
kill -TERM -- -$CHILD 2>/dev/null || true
exit $rc
