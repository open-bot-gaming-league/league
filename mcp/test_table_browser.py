"""The table page in a real (headless) browser: our server and a fake
GitHub in-process (the `site` fixture), the agent here over MCP, a program
here over HTTP with a program token, and table_browser_check.py driving
Chrome in a subprocess, step by step.

Skips itself unless OBGL_BROWSER_PYTHON names a python with Playwright
(OBGL_BROWSER_CHROME: the Chrome to use) and the live table is up.

    OBGL_BROWSER_PYTHON=... python -m pytest mcp/test_table_browser.py -q
"""

import json
import os
import shutil
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio.to_thread
import httpx2
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


needs_browser = [
    pytest.mark.skipif(not BROWSER_PYTHON, reason="OBGL_BROWSER_PYTHON (a python with playwright) not set"),
    pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777"),
]


def _payload(result):
    assert not result.is_error, result.content
    return result.structured_content if result.structured_content is not None else json.loads(result.content[0].text)


def _pick(state):
    kinds = {a["kind"]: a["index"] for a in state["actions"]}
    return kinds.get("check", kinds.get("call", state["actions"][0]["index"]))


class Browser:
    def __init__(self, base):
        self.proc = subprocess.Popen(
            [BROWSER_PYTHON, CHECK, base, os.environ.get("OBGL_BROWSER_SHOTS", "")],
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


class Program:
    """A program at the table, with a program token: the API calls the Go SDK makes."""

    def __init__(self, http, token):
        self.http, self.headers = http, {"Authorization": f"Bearer {token}"}

    async def state(self):
        r = await self.http.get("/table/api/state", params={"brief": "1"}, headers=self.headers)
        assert r.status_code == 200, r.text
        return r.json()

    async def move(self, s):
        if s["over"]:
            r = await self.http.post("/table/api/next", json={"hand": s["hand"]}, headers=self.headers)
        else:
            kind = "check" if s["legal"]["check"] else "call"
            r = await self.http.post("/table/api/act", json={"hand": s["hand"], "step": s["step"], "kind": kind}, headers=self.headers)
        assert r.status_code == 200, r.text
        return r.json()


@pytest.mark.asyncio
@needs_browser[0]
@needs_browser[1]
async def test_the_table_page_with_the_agent_and_a_program_at_it(site):
    async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        s = _payload(await mcp.call_tool("table", {}))  # the account's table, made here

        async def agent_moves(s):
            return _payload(await mcp.call_tool("next_hand" if s["over"] else "act", {} if s["over"] else {"action": _pick(s)}))

        browser = Browser(site.base)
        try:
            await browser.do("PLAY")  # the same table, played in the page
            await browser.do("SIZER")  # detents, snap, the bb box, the keys; the bet sent is the amount shown
            await browser.do("KEYS")  # F, C, N, Enter; A no longer goes all-in
            await browser.do("MEMORY")  # one remembered size per spot, across hands
            await browser.do("RAISE")  # the dock at phone width; a bet off the menu, typed in bb
            # The agent plays the same table from where the page left it; every move shows up on the page, in order.
            hand, step = map(int, await browser.do("STATE"))
            s = _payload(await mcp.call_tool("state", {}))
            assert (s["hand"], s["step"]) == (hand, step)
            for n in range(6):
                s = await agent_moves(s)
                assert "error" not in s and "note" not in s, s
                # The first move lands on a quiet page, which polls every 3 s; once someone else is
                # playing it polls every second, and each move shows within EXPECT's 2.5 s.
                await browser.do(f"{'CATCHUP' if n == 0 else 'EXPECT'} {s['hand']} {s['step']}")
            await browser.do("PRESENCE Your agent is playing")
            # The page's score is the table's: it moves as the agent's hands end.
            assert (await browser.do("SCORE"))[0] == str(s["hand"] - (0 if s["over"] else 1))
            # A program with a token, made in the page, plays it too.
            token = (await browser.do("TOKEN my bot"))[0]
            async with httpx2.AsyncClient(base_url=site.base, timeout=30) as http:
                prog = Program(http, token)
                p = await prog.state()
                assert (p["hand"], p["step"]) == (s["hand"], s["step"])
                for _ in range(6):
                    p = await prog.move(p)
                    await browser.do(f"EXPECT {p['hand']} {p['step']}")
                await browser.do("PRESENCE \u201cmy bot\u201d is playing")
                # The agent, a move behind, is told the program moved first.
                late = _payload(await mcp.call_tool("act", {"action": "call"}))
                assert "already" in late.get("note", "") or "error" in late, late
                await browser.do("REVOKE my bot")
                r = await http.get("/table/api/state", headers=prog.headers)
                assert r.status_code == 401
            s = _payload(await mcp.call_tool("state", {}))
            # Asleep: a page that has nothing to show for a while, or whose tab goes to the background,
            # stops asking the server (counted by the browser) until Resume; a game the agent keeps
            # playing keeps it awake.
            await browser.do("SLEEPER 4")
            for _ in range(5):  # 10 s of play against a 4 s idle limit
                await anyio.sleep(2)
                s = await agent_moves(s)
            await browser.do("AWAKE")
            await browser.do("ASLEEP")  # the agent stopped: idle
            s = await agent_moves(s)  # what happens while it sleeps shows on Resume
            await browser.do(f"RESUME {s['hand']} {s['step']}")
            await browser.do("HIDE")
            s = await agent_moves(s)
            await browser.do("SHOW")
            await browser.do(f"RESUME {s['hand']} {s['step']}")
            await browser.do(f"CATCHUP {s['hand']} {s['step']}")  # the first page is still playing out the moves it was not asked to wait for
            # An old watch link, opened signed out, ends on the account's table after sign-in.
            assert await browser.do("OLDLINK") == [str(s["hand"]), str(s["step"])]
            await browser.do("DONE")
        finally:
            browser.close()


ENGINE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "poker", "engine")


@pytest.mark.asyncio
@needs_browser[0]
@needs_browser[1]
@pytest.mark.skipif(shutil.which("go") is None, reason="no go on PATH")
async def test_watch_your_bot_play_fifty_hands(site, tmp_path):
    """Watch your bot: the SDK's example bot plays 50 hands at full speed with a program token; the page
    replays every one of them, with every move, then catches up; a second run is skipped with Jump to latest."""
    example = str(tmp_path / "example")
    built = await anyio.to_thread.run_sync(lambda: subprocess.run(
        ["go", "build", "-o", example, "./sdk/example"], cwd=ENGINE, capture_output=True, text=True, timeout=300))
    assert built.returncode == 0, built.stderr
    browser = Browser(site.base)
    try:
        await browser.do("PLAY")
        token = (await browser.do("TOKEN my bot"))[0]
        env = {**os.environ, "OBGL_TABLE": f"{site.base}/table/", "OBGL_TOKEN": token}

        async def bot_plays(hands):
            return await anyio.to_thread.run_sync(lambda: subprocess.run(
                [example, "-table", "-hands", str(hands)], env=env, capture_output=True, text=True, timeout=300))

        async with httpx2.AsyncClient(base_url=site.base, timeout=30) as http:
            prog = Program(http, token)
            before = (await prog.state())["hand"]
            run = await bot_plays(50)
            assert run.returncode == 0, run.stdout + run.stderr
            r = await http.get("/table/api/state", params={"since": str(before - 1)}, headers=prog.headers)
            records = [h for h in r.json()["history"] if h["number"] >= before]
            assert len(records) >= 50, len(records)
            spec = ",".join(f"{h['number']}:{len(h['events'])}" for h in records)
            replayed, hands = map(int, await browser.do(f"SHOWN {spec}"))
            assert hands == len(records) and replayed >= len(records) - 2, (replayed, hands)
            # Again, and this time skip to the end.
            async with anyio.create_task_group() as tg:
                tg.start_soon(bot_plays, 40)
                await browser.do("JUMP")
        await browser.do("DONE")
    finally:
        browser.close()
