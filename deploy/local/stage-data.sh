#!/bin/bash
# stage-data.sh: the local bot-data dir (what is staged into
# deploy/cloudrun/data/ before an image build): the four files the Milestone 1
# bots need, named as served.example.json names them, plus served.json.
# Then verifies every file against bot-data.sha256 (in this directory).
#
#   stage-data.sh                   make/refresh the dir, then verify
#   stage-data.sh --write-manifest  (re)write bot-data.sha256 from what is staged
#
# The files are symlinks into the training output dir ($OBGL_BOT_SRC), not
# copies (~850 MB), so verifying matters: the manifest is what says the
# bytes are still the ones that were reviewed.
#
# Required: OBGL_LOCAL_ROOT, OBGL_BOT_SRC (see common.sh).
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
. "$HERE/common.sh"
need OBGL_LOCAL_ROOT
need OBGL_BOT_SRC
DATA=${OBGL_LOCAL_DATA:-$OBGL_LOCAL_ROOT/data}
SRC=$OBGL_BOT_SRC
EXAMPLE=$HERE/../cloudrun/served.example.json
MANIFEST=$HERE/bot-data.sha256

mkdir -p "$DATA"

# staged name <- training-output name. served.example.json names the files
# by bot (data/README.md: the files "need renaming to match when staged").
# Turtle = CFR-BR k=200 1,500 iterations; Ox = plain MCCFR k=2000 1e9.
while read -r staged source; do
  [ -e "$SRC/$source" ] || { echo "stage-data: missing $SRC/$source" >&2; exit 1; }
  ln -sfn "$SRC/$source" "$DATA/$staged"
done <<EOF
abs-200-200-200.bin abs-200-200-200.bin
abs-2000-2000-2000-p3.bin abs-2000-2000-2000-p3.bin
turtle-k200-1500.bp cfrbr-k200-1500.bp
ox-k2000-1e9.bp plain-k2000-1000000000.bp
EOF

# served.json is served.example.json as the repo has it: the ids, names and
# numbers the Milestone 1 docs promise, and the file names staged above.
cp "$EXAMPLE" "$DATA/served.json"

# Every file served.json names must be staged (loadBots would fail at start).
python3 - "$DATA" <<'EOF'
import json, os, sys
d = sys.argv[1]
for bot in json.load(open(os.path.join(d, "served.json"))):
    for key in ("abs", "blueprint"):
        if key in bot and not os.path.exists(os.path.join(d, bot[key])):
            sys.exit(f"stage-data: served.json bot {bot['id']!r} names {bot[key]!r}, not staged in {d}")
EOF

cd "$DATA"
if [ "${1:-}" = "--write-manifest" ]; then
  {
    echo "# sha256 of the bot data for the local OBGL stack (deploy/local/stage-data.sh)."
    echo "# Names are as staged; training-output names: turtle-k200-1500.bp = cfrbr-k200-1500.bp,"
    echo "# ox-k2000-1e9.bp = plain-k2000-1000000000.bp. Check: cd <data dir> && sha256sum -c this-file"
    sha256sum abs-200-200-200.bin abs-2000-2000-2000-p3.bin turtle-k200-1500.bp ox-k2000-1e9.bp served.json
  } > "$MANIFEST"
  echo "stage-data: wrote $MANIFEST"
fi
sha256sum --quiet -c "$MANIFEST" && echo "stage-data: $DATA matches bot-data.sha256"
