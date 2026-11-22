# mcp/ — the Bot Poker MCP server

An MCP server wrapping the Bot Poker table's HTTP API, so an agent can play through seven tools: six wrap the table, one returns the docs. The hosted connector runs this same code; you don't need to run it yourself to play (see the [README](../README.md)).

Two transports, one codebase: **stdio** (default, for Claude Desktop and local use) and **Streamable HTTP** (`OBGL_TRANSPORT=http`, what [deploy/cloudrun/](../deploy/cloudrun/) serves to claude.ai custom connectors). See `server.py`'s module docstring for how the two share one `build_server()` — the only real difference is how a `PokerSession` gets scoped to a caller.

## Files

- `table_client.py` — thin HTTP client for the table API. No game logic.
- `session.py` — hand/step bookkeeping (`PokerSession`), staleness and validation errors, `compute_score()`. No MCP or network framing; the unit tests exercise it directly.
- `server.py` — the MCP server: seven tools (`docs`, `rules`, `new_session`, `state`, `act`, `next_hand`, `score`), two resources (`obgl://docs/AGENTS.md`, `obgl://docs/WAYS-TO-PLAY.md`), over either transport; over HTTP also the docs as web pages (`/`, `/docs/`, `/docs/*.md`).
- `oauth/` — OAuth on the HTTP connector (on when `OBGL_PUBLIC_URL` is set): this server is the authorization server, using the mcp SDK's routes (metadata, dynamic client registration, `/authorize`, `/token`, `/revoke`) around our provider. `/authorize` shows a sign-in page (GitHub, Google, Continue as guest); we mint our own tokens. Start at `oauth/__init__.py` (config, env names), then `provider.py`, `web.py`, `idp.py`, `store.py`.
- `pages.py` — the HTML page shell the landing, docs index and sign-in pages share.
- `oauth_testing.py` — a scripted OAuth client (register, PKCE, the guest button, `/token`) for the tests and `deploy/local/smoke.py`. Not shipped in the image.
- `test_session.py` — unit tests (no network).
- `conftest.py` — the `run_server` fixture (starts `server.py` over HTTP on a free or given port) and `serve_asgi` (an in-process app on a real socket).
- `test_oauth_provider.py` — the OAuth server in-process over ASGI: register, authorize, code, token, refresh, revoke, PKCE, single-use codes, resource binding, the sign-in page, expiry, the stores, env config. No network.
- `test_oauth_upstream.py` — GitHub and Google sign-in against a fake upstream IdP on a local socket.
- `test_oauth_http.py` — `server.py` with OAuth on, over real HTTP: 401 without a token, guest sign-in then tools (and one hand if the table is up), the SDK's own OAuth client discovering everything from the 401, MCP sessions bound to their account, tokens surviving a restart with the file store.
- `test_docs.py` — the `/docs/*` pages (exact file bytes, content types) and the `docs()` tool. Needs no table.
- `test_e2e.py` — runs `server.py` over stdio with the MCP Python client, plays 3 hands, checks `score()` against the table's own history. Skips itself if no table is reachable.
- `test_e2e_http.py` — the same shape, over real Streamable HTTP (`OBGL_TRANSPORT=http`), plus a second test that two concurrent connections don't share poker state — the actual point of per-connection session scoping. Skips itself if no table is reachable.

## Running it locally

Needs Python with the `mcp` package (tested with 2.3.0) and a reachable table; the tests also need `pytest` and `pytest-asyncio`.

```sh
python -m pytest mcp/ -q                            # unit tests + both e2e suites (table-touching tests skip without a table)
python mcp/server.py                                # stdio, for a local MCP client
OBGL_TRANSPORT=http PORT=8080 python mcp/server.py   # Streamable HTTP at :8080/mcp
OBGL_TRANSPORT=http PORT=8080 OBGL_PUBLIC_URL=http://127.0.0.1:8080 python mcp/server.py   # same, OAuth on (guest sign-in)
```

`OBGL_TABLE_URL` sets the table's base URL (default `http://localhost:9777/poker/`). `HOST`/`PORT` only matter for the HTTP transport (defaults `0.0.0.0`/`8080`).
