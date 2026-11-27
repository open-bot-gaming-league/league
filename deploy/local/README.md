# deploy/local: the Cloud Run container, run on the dev box

Local only. Nothing here touches gcloud, credentials, or any remote
(`run.sh` may `git fetch origin` anonymously if a sha is missing).

Setup, once. Two paths are machine-specific and have no default: put them in
the environment or in `deploy/local/local.env` (gitignored, `VAR=value` lines):

```sh
OBGL_LOCAL_ROOT=<dir for trees, venvs, data, state>   # created if missing
OBGL_BOT_SRC=<dir holding the trained bot files>      # see "Bot data" below
```

You also need `go`, `python3` (with `venv`) and `git` on PATH.

```sh
deploy/local/run.sh <sha>     # build + run that sha; Ctrl-C / SIGTERM stops all of it
python deploy/local/smoke.py --url http://127.0.0.1:9792 --repo $OBGL_LOCAL_ROOT/trees/<sha>/context --sha <sha>
```

`run.sh` (1) resolves the sha in the repo, (2) refuses if its two ports are
taken, (3) stages and verifies the bot data (`stage-data.sh`), (4) builds the
image tree once per sha under `$OBGL_LOCAL_ROOT/trees/<sha>/` by running
**this repo's own Dockerfile** through `image.py`, then (5) runs the image's ENTRYPOINT
(`entrypoint.sh`, unmodified except one line, below) with the Dockerfile's ENV.
`image.py` knows only the instructions the Dockerfile uses today and fails on
anything else, so an image change cannot silently go missing locally.

Files: `run.sh`, `common.sh` (env handling), `image.py` (Dockerfile emulator), `stage-data.sh` (bot data),
`bot-data.sha256` (manifest), `smoke.py` (HTTP proof; with OAuth it checks the sign-in page and,
only against a server wired to a test IdP, signs in and plays).

## Bot data and the manifest

`stage-data.sh` makes `$OBGL_LOCAL_ROOT/data/` with the four files
`served.example.json` names, as symlinks into `$OBGL_BOT_SRC`:

| staged name | training-output name |
|---|---|
| `abs-200-200-200.bin` | same |
| `abs-2000-2000-2000-p3.bin` | same |
| `turtle-k200-1500.bp` | `cfrbr-k200-1500.bp` (CFR-BR k=200, 1,500 it) |
| `ox-k2000-1e9.bp` | `plain-k2000-1000000000.bp` (plain MCCFR k=2000, 1e9 it) |

and `served.json` = this sha's-era `served.example.json`, verbatim.
`bot-data.sha256` lists all five; `run.sh` checks it on every start. The repo
had no manifest before this (grep for sha256/manifest/checksum found nothing);
the only numbers it states are sizes (CLOUDRUN.md: ~405 MB x2, ~3.8 MB,
~37.7 MB), which match the files (404,821,288 / 3,792,669 / 37,711,869 bytes).
The four bot files' hashes match the deployed image's byte for byte. `served.json`'s
is the repo's example file at this sha: when `served.example.json` changes, the manifest
line changes with it, and the deployed image's `served.json` matches only once that sha is deployed.
Check any staged data dir with `cd <data dir> && sha256sum -c deploy/local/bot-data.sha256`.

## Where local differs from the image

**Ports and binding**
- MCP port is **9792**, not Cloud Run's `$PORT` (8080 by default): a dev box often
  has other services on the common ports. Override: `OBGL_LOCAL_PORT`.
- The table's private port is **9793**, not 9790: another table on the dev box
  this was written on already holds 9790. `entrypoint.sh` hard-codes `TABLE_ADDR=127.0.0.1:9790`, so run.sh
  `sed`s that one line in the per-sha copy (it must match exactly once, or run.sh stops).
  `OBGL_TABLE_URL` follows, since the entrypoint exports it from `TABLE_ADDR`.
- `OBGL_PUBLIC_URL=http://127.0.0.1:<port>`, not the image's `https://obgl.truemirrorgames.com`:
  it is the OAuth issuer and token audience, so it must be the URL clients actually use.
  No GitHub/Google credentials are set, so the sign-in page offers no way to sign in: smoke checks the page and skips the signed-in steps.
- The OAuth store is the file store `<state dir>/oauth.json`, fresh per run (tokens do not
  outlive a run), not the image's Firestore. `OBGL_LOCAL_AUTH_STORE=firestore` keeps the
  image's Firestore store against a local emulator: it needs `FIRESTORE_EMULATOR_HOST` and
  `GOOGLE_CLOUD_PROJECT` (any `demo-*` id) in the environment, and refuses to start without
  the emulator host (`mcp/test_oauth_store.py` says how to start one).
- Bot submissions are on, in the local file store `<state dir>/bots/` (fresh per run),
  checked by the image's `botcheck`: `OBGL_BOT_STORE=local:...`. The image leaves them off
  (no `OBGL_BOT_STORE`: the API answers 503) until a deploy names a Cloud Storage bucket
  (`deploy/cloudrun/BOT-STORAGE.md`). Uploads go to the server itself here, at `/bots/upload/`;
  in the image they go straight to Cloud Storage.
- `HOST=127.0.0.1` (server.py defaults to 0.0.0.0, which is what Cloud Run gets). No
  TLS, no Cloud Run front end: no HTTPS termination or `X-Forwarded-*`, request timeout,
  concurrency limit, scale-to-zero/cold start, or instance limit.

**Not a container**
- Plain processes as the calling user, no filesystem isolation, no memory/CPU limit (the
  image is sized for 2 GiB), page cache shared with everything else on the box.
- Inherits the caller's environment (Cloud Run's contains only the image ENV plus
  `PORT`, `K_SERVICE`, `K_REVISION`, `K_CONFIGURATION`; the `K_*` are not set here).
- Paths: `/app`, `/src`, `/out`, `/state` are directories under
  `trees/<sha>/fs/<stage>/`; the entrypoint runs with that `app/` as cwd.
- `OBGL_STATE_DIR` is a fresh `state/<sha>-<utc time>/` per run (a new instance);
  old ones are left, not deleted.
- Stopping: SIGTERM kills the process group. In a container, the MCP server dying ends
  the container (and the table with it); `run.sh` does that explicitly.

**Build**
- Go is whatever `go` is on PATH (`go version`), with `GOTOOLCHAIN=local` as
  the golang image sets; the image uses `golang:1.27` (latest 1.27.x, Debian, its gcc/libc).
  Python is the box's system 3.12 in a venv, not `python:3.12-slim`.
  `requirements.txt` pins only `mcp==2.3.0`; transitive packages float, so the local
  venv's versions can differ from the image's: compare `<venv>/bin/pip freeze` with
  the image's `pip freeze` (venvs are keyed by requirements.txt hash under `venvs/`).
- The build context is `git archive <sha>` (tracked files only) plus the staged
  `deploy/cloudrun/data/`. Cloud Build uploads a working tree filtered by `.gcloudignore`,
  so an untracked file there could ship to prod and not here. No `.dockerignore` handling.
- Staged data is **symlinks** into the training dir, not a copy (850 MB per sha): the
  manifest check at start is what guards it. Image data is a copy.
- `OBGL_GIT_SHA` is passed as the Dockerfile's `--build-arg`, the way a real build
  passes it. A sha whose Dockerfile has no such `ARG` (before the /version commit) has no
  `/version` route either, and image.py says so.
- `served.json` is the repo's example file; the deployed image's matches it when the image was built from the same sha (see the manifest).
