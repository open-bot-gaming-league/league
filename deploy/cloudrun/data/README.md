# deploy/cloudrun/data/ -- staged, not committed

Not in git (see ../.gitignore): the bot data the table server needs at
startup is large (hundreds of MB per bucket file) and reproducible
from training, not something a repo should carry.

Before `docker build` (see ../Dockerfile, ../CLOUDRUN.md), stage a copy
here:

- `served.json` -- the bots-on-offer file loadBots reads (see
  `../../poker/engine/cmd/table/main.go`). `served.example.json` one
  level up is the served set (ids `turtle`, `ox`); copy it
  here as `served.json`, or write your own with the same fields
  (`id`, `name`, `trained`, `abs`, `blueprint`; `note`, a line on the
  bot's style, optional) -- `loadBots` refuses a bot missing an `id` or
  `name`.
- The two bucket files and two blueprints `served.json` names (one
  bucket file + one blueprint per bot -- see `served.example.json`).
  The two blueprint files are named `turtle-k200-1500.bp` and
  `ox-k2000-1e9.bp` in `served.example.json` on purpose (by bot, not by training run); the files as they come off
  training have their own, more generic names and need renaming to
  match when staged here.

Ask whoever trained the bots on offer for the current location of
these files (they live on the training box's own output directory,
not in this repo) and for which of their files is which bot's.
`hands.jsonl` is not staged here either way: the table server creates
its session-state file fresh on first run, under its own path
(`$OBGL_STATE_DIR`, default `/state` -- see `../CLOUDRUN.md` and
`../entrypoint.sh`), deliberately not under this directory.
