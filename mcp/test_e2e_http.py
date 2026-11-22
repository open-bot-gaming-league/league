"""End-to-end test: starts server.py as a subprocess with
OBGL_TRANSPORT=http, connects the real MCP Python client over HTTP to
its /mcp endpoint, plays 3 full hands with simple legal actions
(call/check when possible, else the cheapest non-fold action), and
checks score() against what the table's own api/state history says --
not just the wrapper's internal math. Same shape as test_e2e.py (the
stdio transport's e2e test), plus a second test that two concurrent
connections don't share poker state -- the actual point of
_poker_for(ctx)'s per-mcp-session-id keying in server.py.

Needs the live table at http://localhost:9777/poker/ (read-only GETs
plus our own new sessions -- see poker/engine/cmd/table/API.md). Run
with a venv that has the `mcp` package (see requirements.txt):

  python -m pytest mcp/test_e2e_http.py -q
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from table_client import TableClient


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


pytestmark = pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")


def _payload(result):
    """A tool's answer as a plain dict -- same shape games would see
    through a real MCP client (Claude Desktop, claude.ai)."""
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


@pytest.fixture
def http_server(run_server):
    """The MCP endpoint URL of a server.py started on a free port
    (conftest.run_server); the table URL stays at its default, the
    live table at localhost:9777/poker/."""
    with run_server() as base:
        yield f"{base}/mcp"


@pytest.mark.asyncio
async def test_three_hands_end_to_end_and_score_matches_table_history(http_server):
    from mcp import Client

    async with Client(http_server) as client:
        rules = _payload(await client.call_tool("rules", {}))
        assert rules["bigBlind"] == 100
        assert rules["bots"], "rules() should list at least one opponent"
        opponent_id = rules["bots"][0]["id"]

        state = _payload(
            await client.call_tool("new_session", {"opponent": opponent_id, "seat": "alternate"})
        )
        assert "error" not in state
        assert state["hand"] == 1
        table_session_id = state["session"]

        hands_completed = 0
        for _ in range(200):  # a generous cap so a stuck test fails, not hangs
            if hands_completed >= 3:
                break
            if state.get("over"):
                state = _payload(await client.call_tool("next_hand", {}))
                assert "error" not in state, state
                hands_completed += 1
                continue
            action = _pick_action(state["actions"])
            state = _payload(await client.call_tool("act", {"action": action}))
            assert "error" not in state, state

        assert hands_completed == 3

        score = _payload(await client.call_tool("score", {}))
        assert score["hands"] >= 3

    # Check against the table's own history for this session, fetched
    # directly -- not through the server-under-test's own math.
    raw = TableClient().state(table_session_id)
    history = raw["history"]
    assert len(history) == score["hands"]
    assert sum(h["net"] for h in history) == score["net_chips"]
    opponents_seen = {h["opponent"] for h in history}
    assert opponents_seen <= set(score["by_opponent"])


@pytest.mark.asyncio
async def test_two_concurrent_connections_do_not_share_poker_state(http_server):
    """The real point of _poker_for(ctx) in server.py: two separate MCP
    connections (as two different claude.ai conversations would be) must
    not see or disturb each other's session, hand, or score."""
    from mcp import Client

    async with Client(http_server) as client_a, Client(http_server) as client_b:
        rules = _payload(await client_a.call_tool("rules", {}))
        opponent_id = rules["bots"][0]["id"]

        state_a = _payload(
            await client_a.call_tool("new_session", {"opponent": opponent_id, "seat": "alternate"})
        )
        state_b = _payload(
            await client_b.call_tool("new_session", {"opponent": opponent_id, "seat": "alternate"})
        )
        assert state_a["session"] != state_b["session"]

        # Act on A only; B's own state() must be unaffected.
        action = _pick_action(state_a["actions"])
        await client_a.call_tool("act", {"action": action})

        state_b_again = _payload(await client_b.call_tool("state", {}))
        assert state_b_again["session"] == state_b["session"]
        assert state_b_again["hand"] == state_b["hand"]
        assert state_b_again["step"] == state_b["step"]
