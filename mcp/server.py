"""MCP server for Bot Poker: six tools (rules, new_session, state, act,
next_hand, score) wrapping the live table's HTTP API. See
docs/AGENTS.md for what this is and how to read it; see
mcp/SETUP-DESKTOP.md for wiring the stdio mode into Claude Desktop.

Two transports, one server:

- **stdio** (default) -- one process serves exactly one client, so all
  tool calls share a single `PokerSession`. This is what Claude
  Desktop and a local `python server.py` get.
- **Streamable HTTP** (`OBGL_TRANSPORT=http`) -- the transport
  claude.ai custom connectors speak, and what deploy/cloudrun/ serves.
  One running server can hold many concurrent client connections (one
  per claude.ai conversation), each with its own `mcp-session-id`, so
  poker state here is kept per `mcp-session-id` -- see `_poker_for`
  below -- or two different conversations would silently share one
  poker session and stomp each other's hands.

`_poker_for(ctx)` is what makes one `build_server()` correct under
both: it keys a `PokerSession` off the transport's own per-connection
identity when the transport has one (`ctx.headers` is not `None`), and
falls back to a single shared session when it doesn't (stdio has no
headers at all -- `getattr(request, "headers", None)` on its request
object misses -- and stdio only ever has the one client anyway, so a
shared fallback is exactly as correct as the old stdio-only server's
module-level `PokerSession` was).

Tool bodies otherwise do exactly what they always did; summarize_state
and PokerSession live in session.py, table_client.py is the HTTP
client -- neither has anything transport-specific in it.

Run:
  python server.py                                   # stdio (default)
  OBGL_TRANSPORT=http PORT=8080 python server.py      # Streamable HTTP

OBGL_TABLE_URL points at the table; defaults to
http://localhost:9777/poker/ for a local non-container run. Inside the
deploy/cloudrun/ container the entrypoint sets it to the table's own
in-container address.
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mcp.server.mcpserver import Context, MCPServer  # noqa: E402
from mcp.server.streamable_http import MCP_SESSION_ID_HEADER  # noqa: E402
from mcp_types import DiscoverResult, RequestParams  # noqa: E402

from session import (  # noqa: E402
    InvalidActionError,
    NoSessionError,
    PokerSession,
    StaleStateError,
    summarize_state,
)
from table_client import TableClient, TableError  # noqa: E402

DOCS_DIR = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs"))

# A fixed key for transports with no per-connection identity of their
# own (stdio): there is only ever one client, so every tool call shares
# one PokerSession, same as the original stdio-only server.
_SHARED_SESSION_KEY = "shared"

RULES_TEXT = """Bot Poker: heads-up, 50 bb starting stacks, stacks reset every hand.
Blinds: small blind 50 chips (0.5 bb), big blind 100 chips (1 bb) -- "bb"
below always means one big blind, 100 chips. Seats are "small blind
(button)" (acts first before the flop, last after) and "big blind".

Cards are two characters, rank then suit: `As`, `Td`, `2c`. Ranks:
2-9, T, J, Q, K, A. Suits: s, h, d, c. The board shows 0 (before the
flop), 3 (flop), 4 (turn) or 5 (river, and at a showdown/all-in) cards.

The bet menu (from api/info, read live so it stays correct if it
changes): a round's first bet is 0.5x, 1x or 2x pot; a raise is 1x pot;
a re-raise is 1x pot; all-in is always offered. Actions are chosen by
`index` into the `actions` list the last state gave you -- never guess
an index, read it from state().

Each poker tool (state/act/next_hand/score) returns the same compact
shape: hand, step, round, seat, opponent, you, bot, board, pot, over,
actions, lines, and (once a hand is over) outcome and net. `act()` fills
in which hand/step you mean from the last state this session saw, so you
never need to track those yourself -- just call state() if a result
looks unexpected before acting again.
"""


def build_server(base_url: str | None = None) -> MCPServer:
    client = TableClient(base_url) if base_url else TableClient()

    # One PokerSession per connection that has one (see _SHARED_SESSION_KEY
    # and the module docstring for stdio's fallback). `ctx.session` (the
    # SDK's ServerSession) is *not* usable as the per-connection key over
    # HTTP: it is a fresh, per-request proxy object (see mcp/server/
    # session.py's own docstring: "A per-request proxy built by the
    # kernel for each inbound request"), so it differs between two
    # requests on the same connection. The stable, public, per-connection
    # handle over HTTP is the transport's own `mcp-session-id` header
    # (`ctx.headers`), which every SDK client resends on every request
    # after the one that received it from `initialize`'s response.
    poker_by_key: dict[str, PokerSession] = {}  # leaks on idle/disconnect; see deploy/cloudrun/CLOUDRUN.md

    def _poker_for(ctx: Context) -> PokerSession:
        headers = ctx.headers
        if headers is None:
            # No headers at all on this transport's request object
            # (stdio): one process serves one client, so one shared
            # session is correct -- there is nothing to key per-caller.
            key = _SHARED_SESSION_KEY
        else:
            session_id = headers.get(MCP_SESSION_ID_HEADER)
            if not session_id:
                # Only expected with stateless_http=True (not used here)
                # or a non-conformant client. Fail loud rather than
                # silently fusing two callers' poker sessions.
                raise RuntimeError(
                    f"no {MCP_SESSION_ID_HEADER!r} header on this request: cannot "
                    "scope poker state to a connection"
                )
            key = session_id
        poker = poker_by_key.get(key)
        if poker is None:
            poker = PokerSession(client)
            poker_by_key[key] = poker
        return poker

    server = MCPServer(
        name="obgl-bot-poker",
        instructions=(
            "Play Bot Poker (50 bb heads-up) at the live table. Read the "
            "WAYS-TO-PLAY and AGENTS resources first, then call rules(), "
            "then new_session()."
        ),
    )

    # mcp 2.3.0 also speaks a newer "modern" Streamable HTTP mode
    # (protocol version 2026-07-28): a self-contained POST per call, no
    # `initialize` handshake, no `Mcp-Session-Id` -- see
    # mcp/server/_streamable_http_modern.py's module docstring. An
    # auto-negotiating client probes `server/discover` for it before
    # every connection. That mode has no notion of a connection at all,
    # so _poker_for(ctx) above has nothing to key on over HTTP -- the
    # whole "act() fills in hand/step from the last state this session
    # saw" design in session.py is unimplementable without session
    # continuity. Overriding `server/discover` to advertise no modern
    # version is the SDK's documented way for a stateful server to opt
    # out: the client reads that as "legacy advertisement, not an
    # incompatibility" (mcp/client/_probe.py) and falls back to the
    # classic handshake, which does carry `Mcp-Session-Id`. Same
    # mechanism the probe module says the Go and TypeScript SDKs'
    # stateful servers use. Harmless under stdio, which never sends
    # this request.
    async def _discover_legacy_only(ctx, params):  # noqa: ANN001, ARG001
        return DiscoverResult(
            supported_versions=[],
            capabilities=server._lowlevel_server.get_capabilities(protocol_version=ctx.protocol_version),
            instructions=server.instructions,
        )

    server._lowlevel_server.add_request_handler("server/discover", RequestParams, _discover_legacy_only)

    @server.resource(
        "obgl://docs/AGENTS.md",
        name="AGENTS.md",
        description="What this is, the tools, the rules of Bot Poker, notation. Read first.",
        mime_type="text/markdown",
    )
    def agents_doc() -> str:
        with open(os.path.join(DOCS_DIR, "AGENTS.md"), encoding="utf-8") as f:
            return f.read()

    @server.resource(
        "obgl://docs/WAYS-TO-PLAY.md",
        name="WAYS-TO-PLAY.md",
        description="The ladder of ways to play Bot Poker, and where this MCP server sits on it.",
        mime_type="text/markdown",
    )
    def ways_to_play_doc() -> str:
        with open(os.path.join(DOCS_DIR, "WAYS-TO-PLAY.md"), encoding="utf-8") as f:
            return f.read()

    @server.tool()
    def rules() -> dict:
        """The rules of Bot Poker and the current opponents on offer.
        Read obgl://docs/AGENTS.md first; this tool is the live part
        (bet menu, opponent list) that can change under that doc."""
        info = client.info()
        return {
            "text": RULES_TEXT,
            "stackBB": info["stackBB"],
            "smallBlind": info["smallBlind"],
            "bigBlind": info["bigBlind"],
            "menu": info["menu"],
            "bots": info["bots"],
            "docs": ["obgl://docs/AGENTS.md", "obgl://docs/WAYS-TO-PLAY.md"],
        }

    @server.tool()
    def new_session(ctx: Context, opponent: str | None = None, seat: str | None = None) -> dict:
        """Start a new session and deal hand 1. opponent: a bot id from
        rules()'s `bots` (default: the first/least-exploitable). seat:
        "sb", "bb" or "alternate" (default -- alternates seat each
        hand, so neither seat's advantage dominates the score)."""
        try:
            state = _poker_for(ctx).new(opponent=opponent, seat=seat)
        except TableError as e:
            return {"error": e.message}
        return summarize_state(state)

    @server.tool()
    def state(ctx: Context) -> dict:
        """The current hand's state: your cards, the bot's visible
        state, the board, pot, and your legal actions. Call this if a
        result looks unexpected before acting again."""
        try:
            s = _poker_for(ctx).state()
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def act(ctx: Context, action: int) -> dict:
        """Take action `index` from the current state's `actions` list.
        Fills in which hand/step you mean from the last state this
        session saw -- you never pass those yourself."""
        try:
            s = _poker_for(ctx).act(action)
        except (NoSessionError, StaleStateError, InvalidActionError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def next_hand(ctx: Context) -> dict:
        """Deal the next hand. Only works once the current hand is over
        (state().over is true)."""
        try:
            s = _poker_for(ctx).next_hand()
        except (NoSessionError, StaleStateError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def score(ctx: Context) -> dict:
        """hands played, net bb, bb/100, and the same split by
        opponent, computed from this session's hand history."""
        try:
            return _poker_for(ctx).score()
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}

    return server


def main() -> None:
    base_url = os.environ.get("OBGL_TABLE_URL")
    server = build_server(base_url)
    if os.environ.get("OBGL_TRANSPORT", "stdio") == "http":
        host = os.environ.get("HOST", "0.0.0.0")
        port = int(os.environ.get("PORT", "8080"))
        server.run("streamable-http", host=host, port=port, streamable_http_path="/mcp")
    else:
        server.run("stdio")


if __name__ == "__main__":
    main()
