# CLOUDRUN.md -- a Cloud Run server for Bot Poker's MCP connector

Ruling, 2026-11-21 09:44: the engine moves into this repo so there is
one repo to deploy from, not two. Built and tested locally only --
see SAFETY: no `gcloud`, no prod keys, no push to a deploy repo, no
`docker push`, from this branch. Whoever has deploy access reads this,
builds, and deploys it.

## What's here

- `../mcp/server.py` -- the MCP server, **one codebase for both
  transports**: stdio (Claude Desktop, local use) and Streamable HTTP
  (`OBGL_TRANSPORT=http`, what this directory's container serves to
  claude.ai custom connectors). Earlier work on this had the HTTP
  mode as a byte-for-byte-except-header fork of the stdio server,
  with its own copies of `session.py`/`table_client.py` shipped
  alongside it; that duplication is gone -- `mcp/server.py` now holds
  a `_poker_for(ctx)` helper that scopes a `PokerSession` per
  `mcp-session-id` header when the transport has one (HTTP) and falls
  back to one shared session when it doesn't (stdio, which only ever
  has one client anyway). Read `_poker_for`'s docstring and the
  comments around it and the `server/discover` override -- two real
  issues came up building the HTTP mode, both explained in place with
  the fix:
  1. **Per-connection state.** Over stdio, one process serves one
     client. Over HTTP, one running server holds many concurrent
     connections; without scoping state per connection, two different
     claude.ai conversations (or just two browser tabs) would
     silently share one poker session. Fixed by keying on the
     transport's `Mcp-Session-Id` header -- *not* `ctx.session`,
     which turned out to be a fresh object every request even within
     one connection (confirmed by running this the wrong way first
     and watching `act()` lose `new_session()`'s state on the very
     next call).
  2. **A second, incompatible transport mode.** mcp 2.3.0 also speaks
     a newer "modern" Streamable HTTP mode (protocol version
     2026-07-28): one self-contained POST per call, no `initialize`
     handshake, no session id at all. An auto-negotiating client
     probes for it before every connection, and if it had won the
     negotiation here, the tools' whole "no session argument -- state
     lives server-side" design would have been unimplementable, not
     just buggy, because that mode has no notion of a connection.
     Fixed by overriding `server/discover` to advertise no modern
     version, which the client reads as "legacy advertisement" and
     falls back to the classic, session-bearing handshake -- the
     SDK's documented mechanism, and (per its own comments) what the
     Go and TypeScript SDKs' own stateful servers do.
  **This means real risk is still open, not closed by this fix:**
  which mode a real claude.ai custom connector would pick absent the
  override, and whether overriding `server/discover` is itself
  respected by whatever client claude.ai actually runs, have not been
  checked against a real claude.ai connection -- only against this
  SDK's own client and its documented client-side fallback rule.
  Worth confirming this once connected for real, not just trusting the
  comment.
- `../mcp/test_e2e_http.py` -- the same shape as `../mcp/test_e2e.py`
  (3 hands, `score()` checked against the table's own history) but
  over real Streamable HTTP with the MCP Python client, plus a second
  test that two concurrent connections don't share state -- the
  actual point of the per-connection-id fix above. `../mcp/server.py`
  is started with `OBGL_TRANSPORT=http`.
- `Dockerfile`, `entrypoint.sh` -- one container: the Go table server
  and the MCP server (HTTP mode), the MCP tools calling the table on
  `127.0.0.1:9790` inside the container. **Unbuilt**: no `docker` on
  this dev box (checked: `which docker` finds nothing), and this
  project's own policy keeps privileged installs (anything needing
  real privilege to run containers, like a local `docker`) out of
  this box's sandboxed setup -- so this is written and reasoned
  through, not
  run; `go build ./...` and both Python e2e suites were run instead
  (see "Tests run" below), plus the exact command the entrypoint
  would use, against a locally-started table binary.
- `served.example.json`, `data/README.md` -- the Milestone 1 bots
  (ruling, 2026-11-21 03:47): **Turtle** (default, `id: "turtle"`) = CFR-BR,
  k=200, 1,500 iterations, 0.556 bb/hand exact exploitability;
  **Ox** (`id: "ox"`) = plain MCCFR, k=2000, 1e9 iterations, 1.493
  bb/hand exact exploitability. `served.example.json` is the file
  `-bots` needs, with filenames only (no paths from this box); copy
  it to `data/served.json` and stage the four files it names (two
  bucket files, two blueprints) alongside it before building -- see
  `data/README.md`.

- `../../.gcloudignore` -- a Cloud Build/Cloud Run `--source` upload
  reads `.gcloudignore` if one exists at the repo root, or otherwise
  falls back to a generated one that respects `.gitignore` (gcloud's
  own docs). This repo's `deploy/cloudrun/.gitignore` un-ignores
  `data/README.md` but still ignores the rest of `deploy/cloudrun/data/`
  -- exactly the staged bot data the build needs -- so relying on the
  auto-generated fallback would silently upload an empty `data/`
  directory and the Dockerfile's `COPY` would ship nothing, no error
  at build time. The root `.gcloudignore` mirrors the repo's
  `.gitignore` files by hand, with that one exclusion deliberately
  dropped. **Unverified here** (SAFETY: gcloud is not run from this
  box) -- confirm with `gcloud meta list-files-for-upload` before a
  real build, specifically that `deploy/cloudrun/data/`'s staged files
  show up in that list.

## Memory sizing

Turtle and Ox do **not** share a bucket file (Milestone 1's bots use
different abstractions: 200 vs. 2,000 buckets per street), unlike an
earlier two-bot lineup that did -- so this is additive, not shared
page cache: `abs-200-200-200.bin` (~405 MB) + `abs-2000-2000-2000-p3.bin`
(~405 MB) is **~810 MB**, all `mmap`'d (`blueprint.go`, `holdem_abs.go`
-- not a full read into heap), plus the two blueprints (~3.8 MB and
~37.7 MB, both mmap'd) and the Go/Python/uvicorn runtime overhead on
top. **What I can't verify from here:** how tightly Cloud Run's
enforced memory limit accounts for clean, evictable, file-backed mmap
pages versus anonymous memory -- real but soft memory can still
contribute to an OOM kill under a tight limit depending on the
platform's cgroup accounting. **Recommend 2 GiB** to start (not the
~1 GiB a single-bucket-file estimate would have suggested) and watch
Cloud Run's own memory metrics after a real deploy; don't take "810
MB, so 1 GiB should do it" as proven by anything above.

## Session state: local disk vs. Firestore

The table server already has a restart story (`cmd/table/API.md`,
`main.go`): it appends every finished hand to `-hands` (a
`hands.jsonl`-style file) and rebuilds all sessions from that file on
boot (`s.restore`), so a session id "stays good across a restart" and
no one's history or score is lost with it, up to a 200-session LRU
cap.

**That story assumes the hands file's directory survives the
restart**, and it is **not**, automatically, true inside a Cloud Run
container: the container filesystem is ephemeral per instance, and
`--max-instances 1` bounds *concurrent* instances, not restarts -- an
idle, scaled-to-zero service still gets a fresh instance (and an empty
filesystem) on its next cold start.

**This port also separates the hands file from the baked-in bot data**
(`entrypoint.sh`): the Dockerfile bakes `served.json` and the bucket/
blueprint files into `./table/data/`, which this build treats as
read-only, image-baked content; `hands.jsonl` now lives under
`$OBGL_STATE_DIR` (default `/state`), its own path. The earlier
version of this port put both under the same directory, which would
have coupled "where do I mount durable storage for session state" to
"where do I mount a bot-data volume" -- the wrong coupling, since one
is read-mostly, provisioned-at-build-time content and the other is
read-write, grows-forever session history, and a future bot-data
volume mount (see option (a2) below) would otherwise have silently
started shadowing or colliding with `hands.jsonl`.

Three ways to handle session-state durability, in order of how much
they change:

**(a1) Ephemeral local disk, do nothing else.** Simplest: the
Dockerfile and entrypoint as written need no further changes. The
real cost: every scale-to-zero cold start drops all session history
and resets the "session id stays good across a restart" promise
API.md makes -- in practice, under a workload that's actually idle
enough to scale to zero, sessions reset often. Fine if what's wanted
is "a stranger plays a few hands, state can reset whenever nobody's
been playing for a while"; not fine if a persistent running score
across cold starts matters.

**(a2) Local disk + a Cloud Run volume mount backed by Cloud Storage.**
Cloud Run (2nd-gen execution environment) supports mounting a GCS
bucket into the container filesystem. Mount it at `$OBGL_STATE_DIR`
(`/state`) and the existing restore-on-boot/append-as-you-go code
keeps working across cold starts with **zero changes to the Go
server** -- only a deploy-time flag, and now cleanly separate from
the bot-data mount question above. **Unverified, flagged honestly
because it matters:** GCS FUSE's append/fsync semantics aren't
identical to a local filesystem's; a single writer with no concurrent
access (which `max-instances=1` guarantees) is the easy case, but this
has not been run to confirm Go's `os.OpenFile(..., O_APPEND...)`
behaves the same way through the mount. This is the option I'd lean
toward for v0 if cross-restart continuity matters, precisely because
the code changes needed are zero -- but it should be tried against
the real thing before counted on.

**(b) Firestore.** The durable, correctly-concurrent answer, and the
one that stops mattering whether `max-instances` is 1 -- it's what
would let that go above 1 later for real horizontal scale. What
changes in the table server, concretely: `server.sessions` (the
in-memory map) and the hands file both get replaced by reads and
writes to a Firestore collection (one document per session, holding
exactly what `session` holds today: seat mode, opponent, hand/finished
counts, `against`, `history`); `s.restore` becomes a query instead of
a file scan on boot. This pulls in `go.mod`'s first external
dependency ever (`cloud.google.com/go/firestore` -- today's build is
100% stdlib, no `go.sum`, no network needed to build; that stops being
true) and adds a network round-trip to every `act()`/`next_hand()`
call that is today a pure in-memory map lookup -- how much latency
that adds per hand is not measured here. Auth is the one part that's
simple: Cloud Run's attached runtime service account authenticates to
Firestore via Application Default Credentials, no key file, which
keeps this inside the project's existing "one role touches
gcloud/keys" lane naturally -- the Go code just calls the default
client; whoever deploys sets up the service account and IAM binding
at deploy time.

**Recommendation:** (a2) for v0. It's the smallest change (none, to
the Go server), it's cheap (scale-to-zero stays real), and it keeps
the "session id stays good across a restart" promise API.md already
makes. Move to (b) if/when `max-instances` needs to go above 1, or if
(a2)'s GCS FUSE append semantics turn out not to hold up under an
actual test. **Not this port's call to make** -- this is a tradeoff
between "simple now" and "right for scale later," and whoever runs the
real deploy is closer to what GCS FUSE actually does in practice than
I can get from reading source on this box.

## What a deploy would run

Reference only -- not run from here (SAFETY: this project restricts
who touches `gcloud` and deploy keys to one role, not this port).
Fill in `PROJECT`/`REGION`/`BUCKET_NAME`; flags assume (a2) above
(drop the `--add-volume*` flags for (a1), add Firestore IAM instead
for (b)).

```sh
# Stage the bot data (see data/README.md -- ask whoever trained the
# Milestone 1 bots for the current file locations), then build from
# the repo root:
docker build -f deploy/cloudrun/Dockerfile -t obgl-cloudrun:v0 .

# Push to Artifact Registry (the deploying project's own registry):
docker tag obgl-cloudrun:v0 REGION-docker.pkg.dev/PROJECT/obgl/cloudrun:v0
docker push REGION-docker.pkg.dev/PROJECT/obgl/cloudrun:v0

# Deploy. No auth (--allow-unauthenticated): v0 is play money, and the
# tradeoff that buys -- see "No auth" below -- is for whoever owns
# the deploy to accept, not assumed here.
gcloud run deploy obgl-bot-poker \
  --project PROJECT --region REGION \
  --image REGION-docker.pkg.dev/PROJECT/obgl/cloudrun:v0 \
  --port 8080 \
  --memory 2Gi --cpu 1 --cpu-boost \
  --min-instances 0 --max-instances 1 --concurrency 80 \
  --allow-unauthenticated \
  --add-volume name=state,type=cloud-storage,bucket=BUCKET_NAME \
  --add-volume-mount volume=state,mount-path=/state
```

`--cpu-boost`: extra CPU during cold start only, which scale-to-zero
makes frequent; worth it for the mmap fault-in time on the ~810 MB of
bucket files. `--concurrency 80` (the Cloud Run default): fine to
leave as-is now that poker state is scoped per `Mcp-Session-Id` rather
than per process -- multiple concurrent connections to the one
instance no longer cross-talk.

## No auth, for v0

`--allow-unauthenticated` means anyone who has the URL can call the
MCP endpoint directly, not only through claude.ai. What that actually
costs, concretely: play money (no real stakes), bounded compute
(`max-instances=1` means the whole service is at most one instance's
worth of CPU/memory no matter how many people hit it at once), and
bounded session memory (the table server's own 200-session cap evicts
the oldest session rather than erroring, so there's no unbounded
growth -- `main.go`'s `dropOldest`). What it does *not* bound: someone
with the URL can crowd out other sessions by churning past that
200-session cap, and there's no login, so "whoever has the URL" is the
only access control there is. Reasonable for a v0 demo; revisit before
this is the long-term shape.

## Docs: what the service serves, and how to check it after a deploy

`mcp/server.py` (HTTP mode) serves the docs from the files copied into
the image (`README.md` and `docs/`, see the Dockerfile) three ways, so a
client that cannot reach GitHub (claude.ai's fetcher was rate-limited
there, stranger test 11-21) still learns to play:

- `GET /` a one-paragraph landing page with links; `GET /docs/` an index.
- `GET /docs/README.md`, `/docs/AGENTS.md`, `/docs/WAYS-TO-PLAY.md`,
  `Content-Type: text/markdown`, the exact bytes of the repo files.
- MCP tool `docs(name)` with name `readme`, `agents`, `ways-to-play` or
  `rules`; the server instructions tell the agent to call `docs('agents')`
  first.

None of it touches the table, so it also works while the table is down.

**Post-deploy check** (run from a checkout at the deployed sha; the
local suite `python -m pytest mcp/test_docs.py` covers the same ground
against a local server):

1. Bytes match the sha:

   ```sh
   B=https://obgl.truemirrorgames.com
   for f in README.md docs/AGENTS.md docs/WAYS-TO-PLAY.md; do
     curl -fsS "$B/docs/$(basename $f)" | cmp - "$f" && echo "ok $f"
   done
   curl -fsSI "$B/docs/AGENTS.md" | grep -i '^content-type: text/markdown'
   curl -fsS "$B/" | grep -c 'href="/docs/"'
   ```

2. The `docs` tool answers over the real connector (any machine with the
   `mcp` package): connect a `Client("https://obgl.truemirrorgames.com/mcp")`
   and call `docs` with each of the four names, then with `nope` (expect an
   error result that lists the valid names).

3. **The spec test: with GitHub unreachable, a fresh chat still learns to
   play.** claude.ai's own fetcher cannot be firewalled from here, so
   take GitHub out of the picture on the chat side: in a brand-new
   claude.ai chat with web search / fetch turned off and only the OBGL
   connector enabled, do not paste any URL and ask: "How do you play Bot
   Poker with this connector? Then play one hand." Pass when the chat
   (a) answers from the connector (it calls `docs('agents')`, the
   server instructions ask it to), without any github.com fetch in its
   tool log, (b) states the rules correctly (50 bb heads-up, stacks reset
   each hand, the bet menu), and (c) completes a hand. Repeat once with
   web access on and the prompt "Read https://obgl.truemirrorgames.com/docs/AGENTS.md
   and play" to check the web path serves a fetcher too. Not run from the
   dev box (no claude.ai, no deploy access): unverified until Flint runs it.

## Tests run / what's unverified

**`go build ./...`** (`poker/engine`): clean.

**`python -m pytest mcp/ -q`** (venv with `mcp==2.3.0`): all of
`test_session.py` (unit, no network), `test_e2e.py` (stdio transport,
3 hands end-to-end) and `test_e2e_http.py` (Streamable HTTP transport,
3 hands end-to-end + the two-concurrent-connections isolation test)
pass -- 18 passed, against this box's own locally-running table.

**Run, locally, against a table built from this branch's own
`poker/engine`** (not the box's separately-running one -- `go run
./cmd/table -config poker/engine/configs/table_50bb.json -bots
<a local served.json pointing at the bot data files> -addr
127.0.0.1:<port>`, as the brief for this port specified): confirmed
the table loads Turtle and Ox from a `served.json` in
`served.example.json`'s exact shape (both bots load, `api/info` lists
Turtle first, Ox second, with the fields above) and that both e2e
suites pass against it the same way they do against the box's own
table.

**Unverified** (everything that needs `docker`, `gcloud`, or a real
claude.ai connection, none of which this box has or this work
touched):
- The Dockerfile builds at all -- untested, no docker here.
- The container's `entrypoint.sh` health-check loop and the table's
  actual cold-start time with ~810 MB of bucket files mmap'd fresh (no
  page cache warm from a previous run).
- The memory estimate above.
- GCS FUSE append semantics under (a2), if that's the path taken.
- Whether the `server/discover` override actually changes what a real
  claude.ai custom connector negotiates -- verified here only against
  this SDK's own client and its documented fallback rule.
- `OBGL_TABLE_URL=http://127.0.0.1:9790/` (no path prefix) against the
  table binary running un-proxied inside a container -- reasoned from
  `cmd/table`'s own `mux.HandleFunc` routes (root-level, no prefix),
  not run inside an actual container.
