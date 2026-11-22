# mcp/ — the Bot Poker MCP server

An MCP server wrapping the Bot Poker table's HTTP API, so an agent can play through seven tools: six wrap the table, one returns the docs. The hosted connector runs this same code; you don't need to run it yourself to play (see the [README](../README.md)).

Two transports, one codebase: **stdio** (default, for Claude Desktop and local use) and **Streamable HTTP** (`OBGL_TRANSPORT=http`, what [deploy/cloudrun/](../deploy/cloudrun/) serves to claude.ai custom connectors). See `server.py`'s module docstring for how the two share one `build_server()` — the only real difference is how a `PokerSession` gets scoped to a caller.

## Files

- `table_client.py` — thin HTTP client for the table API. No game logic.
- `session.py` — hand/step bookkeeping (`PokerSession`), staleness and validation errors, `compute_score()`. No MCP or network framing; the unit tests exercise it directly.
- `server.py` — the MCP server: seven tools (`docs`, `rules`, `new_session`, `state`, `act`, `next_hand`, `score`), two resources (`obgl://docs/AGENTS.md`, `obgl://docs/WAYS-TO-PLAY.md`), over either transport; over HTTP also the docs as web pages (`/`, `/docs/`, `/docs/*.md`).
- `test_session.py` — unit tests (no network).
- `conftest.py` — the `run_server` fixture: starts `server.py` over HTTP on a free port.
- `test_docs.py` — the `/docs/*` pages (exact file bytes, content types) and the `docs()` tool. Needs no table.
- `test_e2e.py` — runs `server.py` over stdio with the MCP Python client, plays 3 hands, checks `score()` against the table's own history. Skips itself if no table is reachable.
- `test_e2e_http.py` — the same shape, over real Streamable HTTP (`OBGL_TRANSPORT=http`), plus a second test that two concurrent connections don't share poker state — the actual point of per-connection session scoping. Skips itself if no table is reachable.

## Running it locally

Needs Python with the `mcp` package (tested with 2.3.0) and a reachable table.

```sh
python -m pytest mcp/ -q                            # unit tests + both e2e suites (skip without a table)
python mcp/server.py                                # stdio, for a local MCP client
OBGL_TRANSPORT=http PORT=8080 python mcp/server.py   # Streamable HTTP at :8080/mcp
```

`OBGL_TABLE_URL` sets the table's base URL (default `http://localhost:9777/poker/`). `HOST`/`PORT` only matter for the HTTP transport (defaults `0.0.0.0`/`8080`).
