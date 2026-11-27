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
- `delete_account.py` -- the operator's tool for deleting one account's
  Firestore records ("Deleting an account", under OAuth). Not in the image.
- `BOT-STORAGE.md` -- submitted bots' files: why they go straight to Cloud
  Storage, and the bucket, IAM and settings to turn submissions on.

The service answers on `/mcp` (the connector), `/` and `/docs/` (see
"Docs" below), `/version`, the OAuth routes (see "OAuth" below) and
`/table/` (see "Browser table" below).

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

**A known limit: the file only grows.** Every hand ever played adds a line
(about 1 KB), from a person, an agent or a program, and the table reads it
all back at start. On the container's own disk `/state` is in memory and
counts against the 2 GiB limit; a program at its 20 requests a second plays
a few hands a second, hundreds of thousands a day. Nothing rotates or caps
the file. The moves of each hand, which the page replays, are kept only in the
table's memory for a session's latest 200 hands and never written to the
file (`cmd/table` `TestHistorySinceCarriesEventsButNotTheFile` holds a
line to what it was).

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
  ("OAuth" below). Without them the sign-in page offers no way to sign in.

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
bearer token from the service's own OAuth server ("OAuth" below). Sign-in
is GitHub or Google, and client registration (`/register`) is open, as
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
`/token`, `/revoke`, and the sign-in page `/login` with **Continue with
GitHub**, **Continue with Google** (each shown when configured)
and **Cancel**. (Guest sign-in was dropped; tokens it issued no longer work.) GitHub and Google only identify
the person; the service issues its own tokens (access 1 h, refresh 30
days, rotated on refresh). An account is provider + provider user id +
display name (GitHub login; Google `name`). No email is requested
(GitHub: no scope; Google: `openid profile`). claude.ai registers itself
through `/register`.

Privacy: what we keep is stated in
[docs/PRIVACY.md](../../docs/PRIVACY.md) (served at `/docs/privacy`). To
delete an account, see "Deleting an account" below.

### Create

1. **GitHub OAuth App** under the `open-bot-gaming-league` org (Settings >
   Developer settings > OAuth Apps > New; an OAuth App, not a GitHub App):
   - Homepage URL: `https://obgl.truemirrorgames.com`
   - Authorization callback URL: `https://obgl.truemirrorgames.com/login/github/callback`
   - Generate a client secret. No scopes to configure.
2. **Google OAuth client** in `PROJECT`:
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
(by sha256), accounts, each account's table sessions and each session's
owner (`mcp/tables.py`), and program tokens (by sha256) with when each
was last used (`mcp/programs.py`) -- live in Firestore
(`FirestoreStore`, `mcp/oauth/store.py`). Layout: one
collection per record kind, `oauth_<kind>`; document id = the record's
key; fields `record` (the record as a JSON string) and `expires_at` (a
Timestamp, on the kinds that expire). Records past `expires_at` read as
absent. The server reads and writes documents by id only.
`oauth_clients`, `oauth_accounts`, `oauth_table-sessions`,
`oauth_watch-tokens` (no longer written: old watch links), `oauth_account-tables`,
`oauth_program-tokens`, `oauth_account-programs` and `oauth_program-usage`
never expire (no `expires_at`, no TTL policy): a program token lasts until
its owner revokes it. `/register` has no cap, so `oauth_clients`
grows by one document per registration.

Create, in `PROJECT`:

1. Enable the Firestore API (`firestore.googleapis.com`).
2. The database: Native mode, standard edition, id `(default)`, in or
   near the service's region (a database's location cannot be changed
   later):
   `gcloud firestore databases create --project PROJECT --location REGION`.
3. TTL policies on `expires_at` for the collections whose records
   expire:

   ```sh
   for c in oauth_pending oauth_codes oauth_access oauth_refresh; do
     gcloud firestore fields ttls update expires_at --project PROJECT \
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

### Deleting an account

A person asks by email (the address in docs/PRIVACY.md); the operator, with
the project's credentials, deletes. The account id is `<provider>:<id>`:
`github:<numeric user id>`, `google:<sub>`, or, from before guest sign-in was dropped, `guest:<random>`. We hold
no email, so find it in the Firestore console's data view: open
`oauth_accounts`; each document's id is the account id and its `record`
field holds the provider and display name. A leftover guest account cannot be
matched to a person (its display name is "Guest"): delete one only by id,
if the person can supply it.

An account's data, and where it lives (every collection is `oauth_<kind>`,
document fields `record` and, on some, `expires_at`):

| collection | document id | how to find the account's documents |
|---|---|---|
| `oauth_accounts` | the account id | by id |
| `oauth_account-tables` | the account id | by id. Its `sessions` list holds only the newest 20 session ids; do not use it to find them all |
| `oauth_table-sessions` | the table session id | every document whose `record.owner` is the account id: scan the collection (`record` is one unindexed JSON string) |
| `oauth_watch-tokens` | sha256 of the session's `record.watch` | computed from each session found above that has a `watch` (sessions from before one table per login) |
| `oauth_account-programs` | the account id | by id |
| `oauth_program-tokens` | sha256 of the token (not reversible) | scan for `record.account` equal to the account id |
| `oauth_program-usage` | the token's id | scan for `record.account` equal to the account id |
| `oauth_account-bots` | the account id | by id |
| `oauth_bot-submissions` | the submission id | scan for `record.account` equal to the account id |
| `oauth_access`, `oauth_refresh`, `oauth_codes` | sha256 of the token (not reversible) | scan each for `record.subject` equal to the account id |

`oauth_clients` and `oauth_pending` hold no account id and are left alone.

`deploy/cloudrun/delete_account.py` does the scans and the deletes (it
needs `google-cloud-firestore`, as in `mcp/requirements.txt`, and
Application Default Credentials for the project). It is a dry run unless
given `--delete`:

```sh
python deploy/cloudrun/delete_account.py github:12345 --project PROJECT            # lists
python deploy/cloudrun/delete_account.py github:12345 --project PROJECT --delete   # deletes
```

It prints one `collection/document` line per document and exits 1 if
there is no account record. Delete, then check that the dry run now
exits 1 with "no account record". The person's still-valid tokens stop
working at once (their records are gone), except a program token, which
the running server remembers for up to a minute (`mcp/programs.py`,
`CACHE_TTL`). A running server also keeps the account's table session id
until it restarts, so restart it after a deletion; signing in again with GitHub or Google makes a new, empty
account under the same id.

The account's submitted bots' files are in Cloud Storage, not Firestore:
the script prints the `gcloud storage rm` that removes them
(`BOT-STORAGE.md`, "Deleting an account"); run it too.

Not covered: the hands. They are in the game server's `hands.jsonl` on
the container's disk (Session state, above), which nothing prunes by
account; a restart of the service clears it. If `/state` is ever moved to
a Cloud Storage volume, hands then outlive restarts, which changes
what we keep: check that docs/PRIVACY.md still holds before doing it.
Cloud Run's request logs are Google's, kept for its default retention.

### Check

```sh
B=https://obgl.truemirrorgames.com
curl -fsS $B/.well-known/oauth-authorization-server   # issuer $B, endpoints under $B
curl -fsS $B/.well-known/oauth-protected-resource/mcp # resource $B/mcp
curl -s -o /dev/null -w '%{http_code}\n' -X POST $B/mcp  # 401
# The sign-in page through the OAuth flow (no guest option, CSP, consent links,
# Cancel; registers one client with a loopback redirect URI). The signed-in
# steps (initialize + table + act, the table) need a person's account,
# so smoke skips them against a real deployment:
python deploy/local/smoke.py --url $B --repo . --sha <deployed sha> --require oauth,version,docs,table
```

Against a revision deployed with `--no-traffic --tag cand`, set `B` to
the tagged URL and add `--stay-on-base` to the smoke command. The
revision's issuer is still `https://obgl.truemirrorgames.com` (the
metadata lines show it); the flag keeps the table check's sign-in
redirects on `$B`, so every request reaches the revision under test.

Then a person, in claude.ai with a fresh account: add the connector, take
the OAuth option, sign in with GitHub, then with Google; each plays a hand. Deploy the same image again; the connector
still works without signing in again.

## Browser table

`/table/` (`mcp/table_web.py`) serves the table's page, proxied from the
table binary, and the API calls it allows: each account's one table, to
the signed-in owner (cookies) and to the owner's programs (the account's
one program token, `Authorization: Bearer obgl_pt_...`, made, regenerated
and revoked on the page; 20 requests a second). Old watch links (`/table/watch/<token>`) go
to `/table/`. It signs in
through the same OAuth server, as its own client (`obgl-table`,
redirect `<OBGL_PUBLIC_URL>/table/signin/callback`, registered by the
server at start). Its cookies are `Secure` when `OBGL_PUBLIC_URL` is
https. Nothing to create or set beyond "OAuth". Each table's move lock,
who is at it (the last move, who was seen), which table session each
account's table is (read from the store once, then kept), each program
token's rate bucket and lookup, and the last finished hand shown to a page
are in the instance's memory: keep `--max-instances 1`.

Check: the `table` check of `deploy/local/smoke.py` signs in at `/table/`,
reads and resets the table, plays a step, and makes (by regenerating),
uses and revokes the account's program token. It needs a server wired to a test IdP (`--sign-in github`,
as in `mcp/test_smoke.py`), so against a real deployment a person does it:
sign in at `/table/`, play a step, make (or regenerate) the token under Programs and run
`OBGL_TOKEN=<it> go run ./sdk/example -table -hands 3` in `poker/engine`
(the page shows the program's moves), then revoke it.

## Docs: what the service serves

`mcp/server.py` (HTTP mode) serves the docs from the files copied into
the image (`README.md` and `docs/`, see the Dockerfile) three ways, so
a client that cannot reach GitHub still learns to play:

- `GET /` a one-paragraph landing page with links; `GET /docs/` an index.
- `GET /docs/README.md`, `/docs/AGENTS.md`, `/docs/WAYS-TO-PLAY.md`,
  `/docs/PRIVACY.md`, `/docs/TERMS.md`, `Content-Type: text/markdown`, the
  exact bytes of the repo files.
- `GET /docs/privacy` and `/docs/terms`: the Privacy Policy and Terms of
  Service as HTML pages (`text/html`), rendered from `docs/PRIVACY.md` and
  `docs/TERMS.md` by `mcp/markdown_lite.py`. These are the URLs for the
  sign-in providers' consent screens.
- MCP tool `docs(name)` with name `readme`, `agents`, `ways-to-play`,
  `privacy`, `terms` or `rules`; the server instructions tell the agent to call `docs('agents')`
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
   for f in README.md docs/AGENTS.md docs/WAYS-TO-PLAY.md docs/PRIVACY.md docs/TERMS.md; do
     curl -fsS "$B/docs/$(basename $f)" | cmp - "$f" && echo "ok $f"
   done
   curl -fsSI "$B/docs/AGENTS.md" | grep -i '^content-type: text/markdown'
   curl -fsS "$B/" | grep -c 'href="/docs/"'
   curl -fsSI "$B/docs/privacy" | grep -i '^content-type: text/html'
   curl -fsSI "$B/docs/terms" | grep -i '^content-type: text/html'
   ```

3. The connector answers: run `deploy/local/smoke.py` as in "OAuth",
   "Check" for the sign-in page; then a person, signed in at claude.ai, has
   Claude call `docs` with each of the six names, then with `nope` (expect an
   error result that lists the valid names).

4. A person, in claude.ai: in a brand-new chat with web search and
   fetch turned off and only the Open Bot Gaming League connector enabled, paste no URL
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
