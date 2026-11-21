# mcp/ — the Bot Poker MCP server

An MCP server wrapping the Bot Poker table's HTTP API, so an agent can play through six tools. The hosted connector runs this same code; you don't need to run it yourself to play (see the [README](../README.md)).

## Files

- `table_client.py` — thin HTTP client for the table API. No game logic.
- `session.py` — hand/step bookkeeping (`PokerSession`), staleness and validation errors, `compute_score()`. No MCP or network framing; the unit tests exercise it directly.
- `server.py` — the MCP server: six tools (`rules`, `new_session`, `state`, `act`, `next_hand`, `score`) and two resources (`obgl://docs/AGENTS.md`, `obgl://docs/WAYS-TO-PLAY.md`).
- `test_session.py` — unit tests (no network).
- `test_e2e.py` — runs `server.py` over stdio with the MCP Python client, plays 3 hands, checks `score()` against the table's own history. Skips itself if no table is reachable.

## Running it locally

Needs Python with the `mcp` package (tested with 2.3.0) and a reachable table.

```sh
python -m pytest mcp/ -q      # unit tests + e2e (e2e skips without a table)
python mcp/server.py          # the server over stdio, for a local MCP client
```

`OBGL_TABLE_URL` sets the table's base URL.
