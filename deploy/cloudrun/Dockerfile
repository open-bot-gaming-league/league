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
# go.mod declares zero external dependencies (stdlib only, no go.sum to
# restore), so this build needs no network access.
RUN go build -o /out/table ./cmd/table

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
RUN chmod +x ./entrypoint.sh ./table/bin/table

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
# The git sha this image was built from, served at GET /version on the
# MCP server (mcp/server.py). Pass it at build time:
#   docker build --build-arg OBGL_GIT_SHA=$(git rev-parse HEAD) ...
# "unknown" if not passed.
ARG OBGL_GIT_SHA=unknown
ENV OBGL_GIT_SHA=$OBGL_GIT_SHA
ENTRYPOINT ["./entrypoint.sh"]
