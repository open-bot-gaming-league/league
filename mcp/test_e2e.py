"""End-to-end test: starts server.py over stdio with the real MCP Python
client, plays 3 full hands with simple legal actions (call/check when
possible, else the cheapest non-fold action), and checks score() against
what the table's own api/state history says -- not just the wrapper's
internal math.

Needs the live table at http://localhost:9777/poker/ (read-only GETs
plus our own new session -- see API.md). Run with the project's venv:

  python -m pytest mcp/test_e2e.py -q
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from table_client import TableClient

SERVER_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "server.py")
PYTHON = sys.executable


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


pytestmark = pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")


def _payload(result):
    """A tool's answer as a plain dict. mcp 2.3.0's structured-output
    auto-detection does not fire for a bare `-> dict` annotation (no
    schema to build from), so every tool here comes back as one
    TextContent block of JSON -- which is also exactly what a real MCP
    client (Claude Desktop) will see, so this is the realistic path."""
    assert not result.is_error, result.content
    if result.structured_content is not None:
        return result.structured_content
    assert len(result.content) == 1
    return json.loads(result.content[0].text)


def _pick_action(actions):
    """A simple legal action: check if offered, else call, else the
    cheapest bet/allin, else whatever's first. Never folds, so hands run
    to showdown or an all-in rather than ending instantly."""
    by_kind = {a["kind"]: a for a in actions}
    for kind in ("check", "call"):
        if kind in by_kind:
            return by_kind[kind]["index"]
    for kind in ("bet", "allin", "fold"):
        if kind in by_kind:
            return by_kind[kind]["index"]
    return actions[0]["index"]


@pytest.mark.asyncio
async def test_three_hands_end_to_end_and_score_matches_table_history():
    from mcp import ClientSession
    from mcp.client.stdio import StdioServerParameters, stdio_client

    params = StdioServerParameters(command=PYTHON, args=[SERVER_PATH])

    async with stdio_client(params) as (read, write):
        async with ClientSession(read, write) as client_session:
            await client_session.initialize()

            rules = _payload(await client_session.call_tool("rules", {}))
            assert rules["bigBlind"] == 100
            assert rules["bots"], "rules() should list at least one opponent"
            opponent_id = rules["bots"][0]["id"]

            state = _payload(
                await client_session.call_tool(
                    "new_session", {"opponent": opponent_id, "seat": "alternate"}
                )
            )
            assert "error" not in state
            assert state["hand"] == 1
            table_session_id = state["session"]

            hands_completed = 0
            for _ in range(200):  # a generous cap so a stuck test fails, not hangs
                if hands_completed >= 3:
                    break
                if state.get("over"):
                    state = _payload(await client_session.call_tool("next_hand", {}))
                    assert "error" not in state, state
                    hands_completed += 1
                    continue
                action = _pick_action(state["actions"])
                state = _payload(await client_session.call_tool("act", {"action": action}))
                assert "error" not in state, state

            assert hands_completed == 3

            score = _payload(await client_session.call_tool("score", {}))
            assert score["hands"] >= 3

    # Check against the table's own history for this session, fetched
    # directly -- not through the server-under-test's own math.
    raw = TableClient().state(table_session_id)
    history = raw["history"]
    assert len(history) == score["hands"]
    assert sum(h["net"] for h in history) == score["net_chips"]
    opponents_seen = {h["opponent"] for h in history}
    assert opponents_seen <= set(score["by_opponent"])
