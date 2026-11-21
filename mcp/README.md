# obgl/mcp — the MCP server for stranger test 2

A local stdio MCP server wrapping the live Bot Poker table's HTTP API
(`/data/projects-wt/poker-table/poker/engine/cmd/table/API.md`) for a
plain Claude to play through. Built for `STRANGER-TEST-2.md`.

## Files

- `table_client.py` — thin sync HTTP client for the table API. No game
  logic.
- `session.py` — the hand/step bookkeeping (`PokerSession`), the
  staleness/validation errors, and `compute_score()`. No MCP or network
  framing; this is what the unit tests exercise directly.
- `server.py` — the MCP server itself: six tools (`rules`,
  `new_session`, `state`, `act`, `next_hand`, `score`) and two resources
  (`obgl://docs/AGENTS.md`, `obgl://docs/WAYS-TO-PLAY.md`).
- `test_session.py` — unit tests (no network) for the bookkeeping and
  `compute_score()`.
- `test_e2e.py` — starts `server.py` over real stdio with the MCP
  Python client, plays 3 hands, checks `score()` against the table's
  own `api/state` history. Skips itself if the live table is not
  reachable.
- `SETUP-DESKTOP.md` — the `claude_desktop_config.json` snippets (two
  options, both untested against a real Desktop) and exactly what could
  not be verified from this box.
- `PREFLIGHT.md` — describes (does not run) the 20-hand fresh-worker
  check `STRANGER-TEST-2.md` asks for before Sunday's run.

Docs the agent reads: `../docs/AGENTS.md`, `../docs/WAYS-TO-PLAY.md`.

## Running it

Venv: `/data/software/venvs/obgl-mcp` (mcp 2.3.0; see
`/data/software/MANIFEST.md`).

```sh
# all tests (unit + e2e; e2e needs the live table at localhost:9777)
/data/software/venvs/obgl-mcp/bin/python -m pytest obgl/mcp/ -q

# the server itself, stdio, for a manual MCP client
/data/software/venvs/obgl-mcp/bin/python obgl/mcp/server.py
```

`OBGL_TABLE_URL` overrides the table's base URL (default
`http://localhost:9777/poker/`).

## What is measured, not yet

Tokens per 100 hands (`STRANGER-TEST-2.md`'s open question 5) needs an
actual run against a model counting tokens, not something this build
can produce by itself -- each tool's reply is a small dict (see
`session.summarize_state`'s dropped-fields comment for what is cut),
but "small" was not turned into a number here.
