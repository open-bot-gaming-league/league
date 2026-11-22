"""The table's Play and Watch in a real (headless) browser: our server and
a fake GitHub in-process (the `site` fixture), the agent here over MCP, and
table_browser_check.py driving Chrome in a subprocess, step by step.

Skips itself unless OBGL_BROWSER_PYTHON names a python with Playwright
(OBGL_BROWSER_CHROME: the Chrome to use) and the live table is up.

    OBGL_BROWSER_PYTHON=... python -m pytest mcp/test_table_browser.py -q
"""

import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio.to_thread
import pytest

import oauth_testing as ot
from conftest import USER_A
from table_client import TableClient

BROWSER_PYTHON = os.environ.get("OBGL_BROWSER_PYTHON")
CHECK = os.path.join(os.path.dirname(os.path.abspath(__file__)), "table_browser_check.py")


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


def _payload(result):
    assert not result.is_error, result.content
    return result.structured_content if result.structured_content is not None else json.loads(result.content[0].text)


def _pick(state):
    kinds = {a["kind"]: a["index"] for a in state["actions"]}
    return kinds.get("check", kinds.get("call", state["actions"][0]["index"]))


class Browser:
    def __init__(self, base, watch_url):
        self.proc = subprocess.Popen(
            [BROWSER_PYTHON, CHECK, base, watch_url, os.environ.get("OBGL_BROWSER_SHOTS", "")],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
        )

    async def do(self, command: str) -> list[str]:
        def roundtrip():
            self.proc.stdin.write(command + "\n")
            self.proc.stdin.flush()
            return self.proc.stdout.readline().strip()

        answer = await anyio.to_thread.run_sync(roundtrip)
        assert answer.startswith("OK"), f"{command}: {answer or 'no answer (the browser script died)'}"
        return answer.split()[1:]

    def close(self):
        if self.proc.poll() is None:
            self.proc.kill()
        self.proc.wait(timeout=10)


@pytest.mark.asyncio
@pytest.mark.skipif(not BROWSER_PYTHON, reason="OBGL_BROWSER_PYTHON (a python with playwright) not set")
@pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")
async def test_play_watch_take_over_and_tag_in_in_a_browser(site):
    async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        s = _payload(await mcp.call_tool("new_session", {"opponent": "turtle"}))
        while s["over"]:
            s = _payload(await mcp.call_tool("next_hand", {}))
        browser = Browser(site.base, s["watch_url"])
        try:
            await browser.do("PLAY")  # its own session, in Play
            assert await browser.do("WATCH") == [str(s["hand"]), str(s["step"])]
            # Every move the agent makes shows up on the page, in order.
            for _ in range(6):
                s = _payload(await mcp.call_tool("next_hand" if s["over"] else "act", {} if s["over"] else {"action": _pick(s)}))
                assert "error" not in s, s
                await browser.do(f"EXPECT {s['hand']} {s['step']}")
            while s["over"]:
                s = _payload(await mcp.call_tool("next_hand", {}))
                await browser.do(f"EXPECT {s['hand']} {s['step']}")
            # Take over: the browser makes the agent's pending decision; the agent is out.
            hand, step = map(int, await browser.do("TAKEOVER"))
            assert (hand, step) != (s["hand"], s["step"])
            assert "not playing" in _payload(await mcp.call_tool("act", {"action": 0}))["error"]
            # Tag in: the agent claims it, inherits the browser's pending decision, and the page watches.
            await browser.do("TAGIN")
            c = _payload(await mcp.call_tool("claim_session", {"session": s["session"]}))
            assert (c["hand"], c["step"]) == (hand, step)
            d = _payload(await mcp.call_tool("act", {"action": _pick(c)}))
            await browser.do(f"EXPECT {d['hand']} {d['step']}")
            await browser.do("DONE")
        finally:
            browser.close()
