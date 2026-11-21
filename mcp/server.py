"""MCP server (stdio) for a plain Claude to play Bot Poker at the live
50 bb table. See obgl/docs/AGENTS.md for what this is and how to read it;
see obgl/mcp/SETUP-DESKTOP.md for wiring it into Claude Desktop.

Tools: rules(), new_session(), state(), act(action), next_hand(), score().
Every tool returns a small dict, never the table's raw JSON (which also
carries `events`, `totals`, `against`, `finished`, `history` -- useful to
a browser, not to an agent paying per token).
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mcp.server.mcpserver import MCPServer  # noqa: E402

from session import (  # noqa: E402
    InvalidActionError,
    NoSessionError,
    PokerSession,
    StaleStateError,
    summarize_state,
)
from table_client import TableClient, TableError  # noqa: E402

DOCS_DIR = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs"))

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
    poker = PokerSession(client)

    server = MCPServer(
        name="obgl-bot-poker",
        instructions=(
            "Play Bot Poker (50 bb heads-up) at the live table. Read the "
            "WAYS-TO-PLAY and AGENTS resources first, then call rules(), "
            "then new_session()."
        ),
    )

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
    def new_session(opponent: str | None = None, seat: str | None = None) -> dict:
        """Start a new session and deal hand 1. opponent: a bot id from
        rules()'s `bots` (default: the first/least-exploitable). seat:
        "sb", "bb" or "alternate" (default -- alternates seat each
        hand, so neither seat's advantage dominates the score)."""
        try:
            state = poker.new(opponent=opponent, seat=seat)
        except TableError as e:
            return {"error": e.message}
        return summarize_state(state)

    @server.tool()
    def state() -> dict:
        """The current hand's state: your cards, the bot's visible
        state, the board, pot, and your legal actions. Call this if a
        result looks unexpected before acting again."""
        try:
            s = poker.state()
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def act(action: int) -> dict:
        """Take action `index` from the current state's `actions` list.
        Fills in which hand/step you mean from the last state this
        session saw -- you never pass those yourself."""
        try:
            s = poker.act(action)
        except (NoSessionError, StaleStateError, InvalidActionError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def next_hand() -> dict:
        """Deal the next hand. Only works once the current hand is over
        (state().over is true)."""
        try:
            s = poker.next_hand()
        except (NoSessionError, StaleStateError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def score() -> dict:
        """hands played, net bb, bb/100, and the same split by
        opponent, computed from this session's hand history."""
        try:
            return poker.score()
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}

    return server


def main() -> None:
    base_url = os.environ.get("OBGL_TABLE_URL")
    server = build_server(base_url)
    server.run("stdio")


if __name__ == "__main__":
    main()
