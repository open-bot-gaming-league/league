# CLOUDRUN.md -- running Bot Poker's MCP connector on Cloud Run

How to build, deploy and check the container in this directory. To run
the same image tree on a machine without Docker, see
`../local/README.md`.

## What's here

- `Dockerfile`, `entrypoint.sh` -- one container, two processes: the Go
  table server (`poker/engine/cmd/table`) on `127.0.0.1:9790`, never
  exposed outside the container, and the MCP server (`mcp/server.py`,
  Streamable HTTP, `OBGL_TRANSPORT=http`) on `$PORT` (Cloud Run sets
  it; 8080 by default). The entrypoint starts the table, waits up to
  30 s for its `/health`, then runs the MCP server in the foreground.
- `served.example.json`, `data/README.md` -- the bots on offer (Turtle,
  the default, and Ox) and how to stage their data before a build.
- `../../.gcloudignore` -- what a `gcloud` source upload includes. It
  mirrors the repo's `.gitignore` files except that it keeps
  `deploy/cloudrun/data/`, which git ignores and the build needs.

The service answers on `/mcp` (the connector), `/` and `/docs/` (see
"Docs" below) and `/version`.

## How the MCP server handles connections

- `mcp/server.py` is one codebase for stdio and HTTP. Over HTTP each
  connection has its own poker session, keyed on the transport's
  `Mcp-Session-Id` header (`_poker_for`). `mcp/test_e2e_http.py` checks
  that two concurrent connections do not share state.
- The server answers `server/discover` with no sessionless protocol
  version, so a client uses the handshake that carries a session id.
  The tools depend on that id.
- A connection's poker session object stays in the MCP server's memory
  after the connection goes idle or away; it is freed when the instance
  stops.

## Bot data

Not in git. Stage it in `deploy/cloudrun/data/` (`data/README.md`):
`served.json`, two bucket files (`abs-200-200-200.bin` and
`abs-2000-2000-2000-p3.bin`, ~405 MB each) and two blueprints
(`turtle-k200-1500.bp`, ~3.8 MB; `ox-k2000-1e9.bp`, ~37.7 MB). Check
the staged files against the manifest:

```sh
cd deploy/cloudrun/data && sha256sum -c ../../local/bot-data.sha256
```

Before a Cloud Build upload, confirm the data is in it. From the repo
root:

```sh
gcloud meta list-files-for-upload . | grep '^deploy/cloudrun/data/'
```

If that prints nothing, the image would be built with an empty data
directory, and the build itself would not fail.

## Memory

The bucket files and blueprints are `mmap`'d (`blueprint.go`,
`holdem_abs.go`), not read into the heap: ~850 MB mapped in total. The
service runs with a 2 GiB limit. How much of the mapped files Cloud Run
counts against the limit has not been measured; watch the service's
memory utilisation after a deploy before lowering it.

## Session state

The table appends every finished hand to `$OBGL_STATE_DIR/hands.jsonl`
(`/state` in the image) and rebuilds its sessions from that file when
it starts, up to 200 sessions (`poker/engine/cmd/table/API.md`,
`main.go`). The bot data lives
elsewhere (`./table/data/`, baked into the image), so a volume mounted
at `/state` does not hide it.

- **On the container's own disk** (the deploy command below): the file
  lasts as long as the instance. A service that has scaled to zero
  starts its next instance with an empty `/state`, so every session,
  history and score is gone after a cold start.
- **On a Cloud Storage volume**: mount a bucket at `/state` and the
  file survives cold starts. Add to the deploy command:

  ```sh
  --add-volume name=state,type=cloud-storage,bucket=BUCKET_NAME \
  --add-volume-mount volume=state,mount-path=/state
  ```

  The service account then needs object read/write on that bucket.
  This has not been run with this server: test that appends through
  the mount behave before relying on it.

Either way keep `--max-instances 1`: sessions live in one table
process, and the hands file has one writer.

## Build and deploy

Fill in `PROJECT`, `REGION` and `SERVICE_ACCOUNT` (a dedicated service
account; with state on the container's disk it needs no roles).

```sh
# From the repo root, with the bot data staged (above).
SHA=$(git rev-parse HEAD)
TAG=$(git rev-parse --short=7 HEAD)
IMAGE=REGION-docker.pkg.dev/PROJECT/obgl/cloudrun:$TAG

docker build -f deploy/cloudrun/Dockerfile --build-arg OBGL_GIT_SHA=$SHA -t $IMAGE .
docker push $IMAGE

gcloud run deploy obgl-bot-poker \
  --project PROJECT --region REGION \
  --image $IMAGE \
  --service-account SERVICE_ACCOUNT \
  --port 8080 \
  --memory 2Gi --cpu 1 --cpu-boost \
  --min-instances 0 --max-instances 1 --concurrency 80 \
  --allow-unauthenticated
```

Without Docker, Cloud Build can run the same `docker build` step on an
upload of the repo root (`gcloud builds submit`).

- `--build-arg OBGL_GIT_SHA`: the sha `GET /version` reports. Without
  it the service reports `unknown`.
- `--cpu-boost`: extra CPU while an instance starts, when the mapped
  files are first read.
- `--concurrency 80`: the Cloud Run default. Connections to the one
  instance are kept apart by `Mcp-Session-Id`.

To check a revision before it takes traffic, add `--no-traffic --tag
cand` to the deploy, run the post-deploy check against the tagged URL
the command prints, then move traffic:

```sh
gcloud run services update-traffic obgl-bot-poker --project PROJECT --region REGION --to-latest
```

To go back to an earlier revision:

```sh
gcloud run services update-traffic obgl-bot-poker --project PROJECT --region REGION --to-revisions REVISION=100
```

## Access

`--allow-unauthenticated`: there is no login. Anyone who has the URL
can call the MCP endpoint, through claude.ai or directly. What bounds
it: the game is play money; `--max-instances 1` caps compute at one
instance; the table keeps at most 200 sessions and drops the oldest
when a new one would exceed that (`main.go`, `dropOldest`). What does
not: a caller who opens sessions fast enough pushes other people's
sessions out of that 200.

## Docs: what the service serves

`mcp/server.py` (HTTP mode) serves the docs from the files copied into
the image (`README.md` and `docs/`, see the Dockerfile) three ways, so
a client that cannot reach GitHub still learns to play:

- `GET /` a one-paragraph landing page with links; `GET /docs/` an index.
- `GET /docs/README.md`, `/docs/AGENTS.md`, `/docs/WAYS-TO-PLAY.md`,
  `Content-Type: text/markdown`, the exact bytes of the repo files.
- MCP tool `docs(name)` with name `readme`, `agents`, `ways-to-play` or
  `rules`; the server instructions tell the agent to call `docs('agents')`
  first.

None of it touches the table, so it also works while the table is down.

## Post-deploy check

Run from a checkout at the deployed sha. (`python -m pytest
mcp/test_docs.py mcp/test_version.py` covers 1 and 2 against a local
server.)

1. The service runs the sha you deployed:

   ```sh
   B=https://obgl.truemirrorgames.com
   curl -fsS "$B/version"    # {"sha":"<the deployed sha>"}
   ```

2. The docs match that sha, byte for byte:

   ```sh
   for f in README.md docs/AGENTS.md docs/WAYS-TO-PLAY.md; do
     curl -fsS "$B/docs/$(basename $f)" | cmp - "$f" && echo "ok $f"
   done
   curl -fsSI "$B/docs/AGENTS.md" | grep -i '^content-type: text/markdown'
   curl -fsS "$B/" | grep -c 'href="/docs/"'
   ```

3. The connector answers (any machine with the `mcp` package): connect
   a `Client("https://obgl.truemirrorgames.com/mcp")`, call `docs` with
   each of the four names, then with `nope` (expect an error result
   that lists the valid names), then `new_session` (expect a dealt
   hand).

4. A person, in claude.ai: in a brand-new chat with web search and
   fetch turned off and only the OBGL connector enabled, paste no URL
   and ask: "How do you play Bot Poker with this connector? Then play
   one hand." Pass when the chat (a) answers from the connector (it
   calls `docs('agents')`), with no github.com fetch in its tool log,
   (b) states the rules correctly (50 bb heads-up, stacks reset each
   hand, the bet menu), and (c) completes a hand. Repeat once with web
   access on and the prompt "Read
   https://obgl.truemirrorgames.com/docs/AGENTS.md and play".

## Tests

- `poker/engine`: `go build ./...` and `go test ./...`
  (`poker/engine/README.md`).
- `mcp/`: `python -m pytest mcp/ -q` (`mcp/README.md`; the end-to-end
  suites skip without a running table).
- The whole image tree at a sha, without Docker: `deploy/local/README.md`.
