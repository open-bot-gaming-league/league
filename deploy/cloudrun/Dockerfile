# deploy/cloudrun/Dockerfile: one container for Cloud Run -- the Go
# table server and the MCP server (Streamable HTTP mode), the MCP
# tools calling the table on 127.0.0.1. CLOUDRUN.md says how to build,
# deploy and check it.
#
# Build from the REPO ROOT (not from this directory), so poker/engine,
# docs/ and mcp/ are all reachable as COPY sources:
#
#   docker build -f deploy/cloudrun/Dockerfile -t obgl-cloudrun .
#
# Precondition on the build context (data/README.md spells this out):
# deploy/cloudrun/data/ (gitignored) must hold served.json and the bot
# files it names before this build runs -- see data/README.md for
# what to stage and served.example.json for its shape.

# ---- stage 1: build the Go table server ----------------------------------
FROM golang:1.27 AS tablebuilder
WORKDIR /src
COPY poker/engine/go.mod ./
COPY poker/engine/*.go ./
# The engine embeds the served config (engine.TableConfig).
COPY poker/engine/configs/table_50bb.json ./configs/table_50bb.json
COPY poker/engine/cmd/table ./cmd/table
# The results pages (cmd/table/results.go) use package league, which uses
# the sdk and its house bots.
COPY poker/engine/sdk ./sdk
COPY poker/engine/league ./league
# go.mod declares zero external dependencies (stdlib only, no go.sum to
# restore), so this build needs no network access.
RUN go build -o /out/table ./cmd/table

# ---- stage 1b: build botcheck, the check of a submitted bot's module ------
# (mcp/bots.py runs it on every submission: arena.Compile, the arena's own
# check against the guest ABI.) The arena module needs the engine's (a
# replace directive: ../engine), and downloads wasmtime-go, checked against
# go.sum; wasmtime-go links its bundled wasmtime library with cgo (the
# golang image has gcc).
FROM golang:1.27 AS arenabuilder
WORKDIR /src
COPY poker/engine/ ./engine/
COPY poker/arena/ ./arena/
WORKDIR /src/arena
RUN go build -o /out/botcheck ./cmd/botcheck
# rankedtable plays a ranked table (mcp/ranked.py's runner runs it).
RUN go build -o /out/rankedtable ./cmd/rankedtable

# ---- stage 1c: the house bots as wasm guests, for ranked tables ----------
# (poker/arena/guests: the starters, and the house guest Turtle and Ox
# share), built once here so a table compiles them but builds nothing.
FROM golang:1.27 AS guestbuilder
WORKDIR /src
COPY poker/engine/ ./engine/
COPY poker/arena/ ./arena/
WORKDIR /src/arena
ENV GOOS=wasip1 GOARCH=wasm
RUN go build -buildmode=c-shared -o /out/guests/dove.wasm ./guests/dove
RUN go build -buildmode=c-shared -o /out/guests/clam.wasm ./guests/clam
RUN go build -buildmode=c-shared -o /out/guests/shark.wasm ./guests/shark
RUN go build -buildmode=c-shared -o /out/guests/house.wasm ./guests/house

# ---- stage 2: the runtime image -------------------------------------------
FROM python:3.12-slim AS runtime
WORKDIR /app

# Python side: the MCP server, run in Streamable HTTP mode
# (OBGL_TRANSPORT=http, set below) -- same code mcp/ ships for stdio.
# requirements.txt is a lock (mcp/requirements.in says how it is made):
# every package pinned with its sha256, so pip runs in hash-checking mode
# and installs exactly those files or fails.
COPY mcp/requirements.txt ./mcp/requirements.txt
RUN pip install --no-cache-dir -r ./mcp/requirements.txt
COPY mcp/server.py mcp/session.py mcp/table_client.py mcp/pages.py mcp/markdown_lite.py mcp/tables.py mcp/programs.py mcp/table_web.py ./mcp/
# Submitted bots (mcp/bots.py; BOT-STORAGE.md): off until OBGL_BOT_STORE is set.
COPY mcp/bots.py mcp/bot_web.py mcp/bot_files.py mcp/bot_files_gcs.py ./mcp/
COPY --from=arenabuilder /out/botcheck ./arena/bin/botcheck
# Checking submissions (mcp/finalize.py): the same image, run as the Cloud
# Run job `python ./mcp/finalize.py run`, which the service starts
# (mcp/jobs.py). Ranked tables (mcp/ranked.py; RANKED-TABLES.md): queued
# when a version is filed; the same image, run as the Cloud Run job
# `python ./mcp/ranked.py run`, plays them with rankedtable, the guests,
# and the house bots' data below.
COPY mcp/finalize.py mcp/jobs.py mcp/ranked.py mcp/badges.py ./mcp/
COPY --from=arenabuilder /out/rankedtable ./arena/bin/rankedtable
COPY --from=guestbuilder /out/guests/ ./arena/guests/
# OAuth on the connector (mcp/oauth/__init__.py; CLOUDRUN.md's OAuth section).
COPY mcp/oauth/ ./mcp/oauth/
# server.py reads the docs from ../docs and ../README.md relative to its
# own file (./docs and ./README.md here, since server.py lives in ./mcp)
# and serves them three ways: the obgl://docs/ resources, the docs() tool,
# and GET /docs/*.md on the web (https://obgl.truemirrorgames.com/docs/).
COPY README.md ./README.md
COPY docs/ ./docs/

# Go side: the table binary, its betting-tree config, and the bot data
# (bucket files + blueprints + served.json) staged into the build
# context per the precondition above.
COPY --from=tablebuilder /out/table ./table/bin/table
COPY poker/engine/configs/table_50bb.json ./table/configs/table_50bb.json
COPY deploy/cloudrun/data/ ./table/data/

COPY deploy/cloudrun/entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh ./table/bin/table ./arena/bin/botcheck ./arena/bin/rankedtable
# Run the arena's two programs once, here, so a bad cgo link of wasmtime
# against this image's libraries (or a guest that does not load) fails the
# build instead of the first submission: botcheck on the house guest, and a
# tiny all-wasm table of the starters (a few seconds).
RUN ./arena/bin/botcheck ./arena/guests/house.wasm
RUN ./arena/bin/rankedtable play -challenger dove -wasm-pool -guests ./arena/guests -pool clam,shark -seats 2 -hands 200 -out /dev/null

# Cloud Run sets $PORT and expects the container to listen on it;
# entrypoint.sh reads it for the MCP server and sets OBGL_TRANSPORT=http
# for it. The table server only ever listens on 127.0.0.1:9790, inside
# the container -- nothing external reaches it directly: the browser table
# at /table is the MCP server's (mcp/table_web.py), which proxies the page
# and the API calls it allows, as the signed-in owner.
ENV OBGL_TABLE_URL=http://127.0.0.1:9790/
ENV OBGL_TRANSPORT=http
# Session state (hands.jsonl) lives under its own path, separate from
# the baked-in bot data under ./table/data/ -- see entrypoint.sh and
# CLOUDRUN.md's "Session state" section for why.
ENV OBGL_STATE_DIR=/state
# OAuth: the public origin is the token issuer and turns OAuth on (/mcp
# then needs a bearer token; the sign-in page offers GitHub and
# Google). Tokens, clients, accounts and table-session owners go in
# Firestore (mcp/oauth/store.py): the project's (default) database, found
# through the Cloud Run service account (ADC; set GOOGLE_CLOUD_PROJECT to
# point elsewhere). Firestore is also the default whenever
# OBGL_PUBLIC_URL is set. The GitHub/Google app credentials
# are NOT baked in: the deploy sets OBGL_GITHUB_CLIENT_ID/_SECRET and
# OBGL_GOOGLE_CLIENT_ID/_SECRET from Secret Manager (CLOUDRUN.md).
ENV OBGL_PUBLIC_URL=https://obgl.truemirrorgames.com
ENV OBGL_AUTH_STORE=firestore
# Bot submissions: the module check is in the image; where the files go
# (OBGL_BOT_STORE=gcs:<bucket>) is the deploy's to set, and until it does
# the submission API answers 503.
ENV OBGL_BOTCHECK=/app/arena/bin/botcheck
# Ranked tables: what the runner plays them with. Where the records go
# (OBGL_RECORDS, also the table's -records) and where the bots' files are
# (OBGL_BOT_FILES_DIR) are the deploy's: RANKED-TABLES.md. The house bots'
# data is the table's own (./table/data, named as sdk.Houses names them).
ENV OBGL_RANKEDTABLE=/app/arena/bin/rankedtable
ENV OBGL_RANKED_GUESTS=/app/arena/guests
ENV OBGL_HOUSE_CACHE=/app/table/data
# The git sha this image was built from, served at GET /version on the
# MCP server (mcp/server.py). Pass it at build time:
#   docker build --build-arg OBGL_GIT_SHA=$(git rev-parse HEAD) ...
# "unknown" if not passed.
ARG OBGL_GIT_SHA=unknown
ENV OBGL_GIT_SHA=$OBGL_GIT_SHA
ENTRYPOINT ["./entrypoint.sh"]
