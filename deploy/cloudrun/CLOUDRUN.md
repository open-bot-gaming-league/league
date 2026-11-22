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
"Docs" below), `/version`, and the OAuth routes (see "OAuth" below).

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
  --allow-unauthenticated \
  --set-secrets OBGL_GITHUB_CLIENT_ID=obgl-github-client-id:latest,OBGL_GITHUB_CLIENT_SECRET=obgl-github-client-secret:latest,OBGL_GOOGLE_CLIENT_ID=obgl-google-client-id:latest,OBGL_GOOGLE_CLIENT_SECRET=obgl-google-client-secret:latest
```

Without Docker, Cloud Build can run the same `docker build` step on an
upload of the repo root (`gcloud builds submit`).

- `--build-arg OBGL_GIT_SHA`: the sha `GET /version` reports. Without
  it the service reports `unknown`.
- `--cpu-boost`: extra CPU while an instance starts, when the mapped
  files are first read.
- `--concurrency 80`: the Cloud Run default. Connections to the one
  instance are kept apart by `Mcp-Session-Id`.
- `--allow-unauthenticated`: no Cloud Run IAM check; the service signs
  users in itself ("Access", "OAuth").
- `--set-secrets`: the GitHub and Google OAuth apps' credentials
  ("OAuth" below). Without them the sign-in page offers guest only.

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

`--allow-unauthenticated` turns off Cloud Run's IAM check: claude.ai and
browsers reach the service without a Google identity. With
`OBGL_PUBLIC_URL` set (the image sets it), `/mcp` answers 401 without a
bearer token from the service's own OAuth server ("OAuth" below). Guest
sign-in is one click, and client registration (`/register`) is open, as
claude.ai needs it. What bounds use: the game is play money;
`--max-instances 1` caps compute at one instance; the table keeps at
most 200 sessions and drops the oldest when a new one would exceed that
(`main.go`, `dropOldest`). What does not: a caller who opens sessions
fast enough pushes other people's sessions out of that 200, and the
number of registered clients has no cap.

## OAuth

The MCP server is the OAuth authorization server for the connector
(code: `mcp/oauth/`, start at `__init__.py`). Routes:
`/.well-known/oauth-authorization-server`,
`/.well-known/oauth-protected-resource/mcp`, `/register`, `/authorize`,
`/token`, `/revoke`, and the sign-in page `/login` with **Sign in with
GitHub**, **Sign in with Google** (each shown when configured),
**Continue as guest** and **Cancel**. GitHub and Google only identify
the person; the service issues its own tokens (access 1 h, refresh 30
days, rotated on refresh). An account is provider + provider user id +
display name (GitHub login; Google `name`). No email is requested
(GitHub: no scope; Google: `openid profile`). claude.ai registers itself
through `/register`.

Privacy: what an account stores is listed above.

### Create

1. **GitHub OAuth App** under the `open-bot-gaming-league` org (Settings >
   Developer settings > OAuth Apps > New; an OAuth App, not a GitHub App):
   - Homepage URL: `https://obgl.truemirrorgames.com`
   - Authorization callback URL: `https://obgl.truemirrorgames.com/login/github/callback`
   - Generate a client secret. No scopes to configure.
2. **Google OAuth client** in the `tmg-obgl` project:
   - OAuth consent screen: External; app name, support email, the
     `https://obgl.truemirrorgames.com` domain; scopes `openid` and
     `profile` only. Publish it ("In production"); otherwise only listed
     test users can sign in.
   - Credentials > Create OAuth client ID > Web application; Authorized
     redirect URI: `https://obgl.truemirrorgames.com/login/google/callback`.
3. **Secret Manager** (same project as the service), one secret each:
   `obgl-github-client-id`, `obgl-github-client-secret`,
   `obgl-google-client-id`, `obgl-google-client-secret`. Grant the
   service account `roles/secretmanager.secretAccessor` on them.
4. **Deploy** with `--set-secrets` as in "Build and deploy".

### Environment

| name | set by | meaning |
|---|---|---|
| `OBGL_PUBLIC_URL` | Dockerfile: `https://obgl.truemirrorgames.com` | the issuer and token audience (`<it>/mcp`); set = OAuth on. `--update-env-vars OBGL_PUBLIC_URL=` turns OAuth off without a rebuild |
| `OBGL_AUTH_STORE` | Dockerfile: `firestore` | `firestore`, `file` or `memory`; `firestore` is also the default when `OBGL_PUBLIC_URL` is set |
| `GOOGLE_CLOUD_PROJECT` | unset | Firestore's project; default: the one Application Default Credentials report |
| `FIRESTORE_EMULATOR_HOST` | unset | tests and `deploy/local` only: a local Firestore emulator. Never set on the service |
| `OBGL_AUTH_FILE` | unset | file store only: its path; default `$OBGL_STATE_DIR/oauth.json` |
| `OBGL_GITHUB_CLIENT_ID`, `OBGL_GITHUB_CLIENT_SECRET` | Secret Manager | both or neither; neither = no GitHub button |
| `OBGL_GOOGLE_CLIENT_ID`, `OBGL_GOOGLE_CLIENT_SECRET` | Secret Manager | same, for Google |
| `OBGL_GITHUB_REDIRECT_URI`, `OBGL_GOOGLE_REDIRECT_URI` | unset | default `<OBGL_PUBLIC_URL>/login/<github\|google>/callback`; the path must be exactly that |

### Storage: Firestore

The OAuth records -- registered clients, pending sign-ins, codes, tokens
(by sha256), accounts, and which account owns each table session -- live
in Firestore (`FirestoreStore`, `mcp/oauth/store.py`). Layout: one
collection per record kind, `oauth_<kind>`; document id = the record's
key; fields `record` (the record as a JSON string) and `expires_at` (a
Timestamp, on the kinds that expire). Records past `expires_at` read as
absent. The server reads and writes documents by id only.

Create, in `tmg-obgl`:

1. Enable the Firestore API (`firestore.googleapis.com`).
2. The database: Native mode, standard edition, id `(default)`, in or
   near the service's region (a database's location cannot be changed
   later):
   `gcloud firestore databases create --project tmg-obgl --location REGION`.
3. TTL policies on `expires_at` for the collections whose records
   expire:

   ```sh
   for c in oauth_pending oauth_codes oauth_access oauth_refresh; do
     gcloud firestore fields ttls update expires_at --project tmg-obgl \
       --collection-group=$c --enable-ttl
   done
   ```

4. Index exemptions: for every `oauth_*` collection group, exempt
   `record` and `expires_at` from indexing
   (`gcloud firestore indexes fields update <field> --collection-group=<c>
   --disable-indexes`).
5. IAM: the service account gets `roles/datastore.user` on the project.
   The server finds the project and credentials through Application
   Default Credentials; no key file or env var.

Check: after a sign-in, `oauth_accounts` and `oauth_clients` hold
documents (the Firestore console's data view). Deploy the same image
again; the connector still works without signing in again.

### Check

```sh
B=https://obgl.truemirrorgames.com
curl -fsS $B/.well-known/oauth-authorization-server   # issuer $B, endpoints under $B
curl -fsS $B/.well-known/oauth-protected-resource/mcp # resource $B/mcp
curl -s -o /dev/null -w '%{http_code}\n' -X POST $B/mcp  # 401
# A guest sign-in through the OAuth flow, then initialize + new_session + act
# with its token (registers one client with a loopback redirect URI):
python deploy/local/smoke.py --url $B --repo . --sha <deployed sha> --require oauth,version,docs
```

Then a person, in claude.ai with a fresh account: add the connector, take
the OAuth option, sign in with GitHub, then with Google, then as a
guest; each plays a hand. Deploy the same image again; the connector
still works without signing in again.

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

3. The connector answers: run `deploy/local/smoke.py` as in "OAuth",
   "Check". With the guest token it gets, a `Client` on `$B/mcp` can
   call `docs` with each of the four names, then with `nope` (expect an
   error result that lists the valid names).

4. A person, in claude.ai: in a brand-new chat with web search and
   fetch turned off and only the OBGL connector enabled, paste no URL
   and ask: "How do you play Bot Poker with this connector? Then play
   one hand." Pass when the chat (a) answers from the connector (it
   calls `docs('agents')`), with no github.com fetch in its tool log,
   (b) states the rules correctly (50 bb heads-up, stacks reset each
   hand, any bet size from the minimum raise to all-in), and (c)
   completes a hand. Repeat once with web access on and the prompt "Read
   https://obgl.truemirrorgames.com/docs/AGENTS.md and play".

## Tests

- `poker/engine`: `go build ./...` and `go test ./...`
  (`poker/engine/README.md`).
- `mcp/`: `python -m pytest mcp/ -q` (`mcp/README.md`; the end-to-end
  suites skip without a running table, the Firestore cases without
  `FIRESTORE_EMULATOR_HOST`; `mcp/test_oauth_store.py` says how to start
  the emulator).
- The whole image tree at a sha, without Docker: `deploy/local/README.md`.
