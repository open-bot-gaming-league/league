# deploy/cloudrun/data/ -- staged, not committed

Not in git (see ../.gitignore): the bot data the table server needs at
startup is large (hundreds of MB per bucket file) and reproducible
from training, not something a repo should carry.

Before `docker build` (see ../Dockerfile, ../CLOUDRUN.md), stage a copy
here:

- `served.json` -- the bots-on-offer file loadBots reads (see
  `../../poker/engine/cmd/table/main.go`). `served.example.json` one
  level up names the Milestone 1 shape (ids `turtle`, `ox`); copy it
  here as `served.json`, or write your own with the same fields
  (`id`, `name`, `trained`, `exploitability`, `abs`, `blueprint`;
  `note` optional) -- `loadBots` refuses a bot missing an `id`, `name`
  or `exploitability`.
- The two bucket files and two blueprints `served.json` names (as of
  Milestone 1: one bucket file + one blueprint per bot -- see
  `served.example.json`).

Ask whoever trained the bots on offer for the current location of
these files (they live on the training box's own output directory,
not in this repo); `served.example.json`'s filenames are what to look
for there. `hands.jsonl` is not staged here either way: the table
server creates its session-state file fresh on first run, under its
own path (`$OBGL_STATE_DIR`, default `/state` -- see `../CLOUDRUN.md`
and `../entrypoint.sh`), deliberately not under this directory.
