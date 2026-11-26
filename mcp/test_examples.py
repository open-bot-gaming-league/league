"""The onboarding examples (examples/: Python, JavaScript, TypeScript) against
this server: each plays three hands of the account's table with a program
token made by the signed-in page, as a new coach's first script would.
JavaScript and TypeScript need node on PATH (TypeScript: a node that strips
types, 22.18+); each skips without its runtime. Skips without the live table.
"""

import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio.to_thread
import pytest

from conftest import USER_A
from table_client import TableClient

EXAMPLES = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "examples")


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


def _node_strips_types() -> bool:
    node = shutil.which("node")
    if not node:
        return False
    major, minor = (int(x) for x in re.findall(r"\d+", subprocess.run([node, "--version"], capture_output=True, text=True).stdout)[:2])
    return (major, minor) >= (22, 18)


RUNS = [
    pytest.param([sys.executable, os.path.join(EXAMPLES, "python/call_bot.py")], id="python"),
    pytest.param(["node", os.path.join(EXAMPLES, "javascript/call_bot.mjs")], id="javascript",
                 marks=pytest.mark.skipif(shutil.which("node") is None, reason="no node on PATH")),
    pytest.param(["node", os.path.join(EXAMPLES, "typescript/call_bot.ts")], id="typescript",
                 marks=pytest.mark.skipif(not _node_strips_types(), reason="no node 22.18+ (type stripping) on PATH")),
]


@pytest.mark.asyncio
@pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")
@pytest.mark.parametrize("how", ["env", "file"])
@pytest.mark.parametrize("command", RUNS)
async def test_an_onboarding_example_plays_the_accounts_table(site, command, how, tmp_path):
    """The token from OBGL_TOKEN (even with another token's file beside it:
    the variable comes first), or from obgl-token.txt in a folder above the
    one it runs in, as the table page's download is saved."""
    async with site.browser(USER_A) as browser:
        token = (await browser.post("/table/api/token", json={"label": "example"})).json()["token"]
        before = (await browser.get("/table/api/state")).json()
        work = tmp_path / "workspace" / "bot"
        work.mkdir(parents=True)
        env = {k: v for k, v in os.environ.items() if k != "OBGL_TOKEN"}
        env["OBGL_TABLE"] = f"{site.base}/table/"
        if how == "env":
            env["OBGL_TOKEN"] = token
            (work / "obgl-token.txt").write_text("obgl_pt_not_this_one\n")
        else:
            (tmp_path / "workspace" / "obgl-token.txt").write_text(token + "\n")
        played = await anyio.to_thread.run_sync(lambda: subprocess.run(
            [*command, "3"], cwd=work, env=env, capture_output=True, text=True, timeout=120))
        assert played.returncode == 0, played.stdout + played.stderr
        lines = played.stdout.splitlines()
        assert sum(line.startswith("hand ") for line in lines) == 3 and lines[-1].startswith("score: "), played.stdout
        after = (await browser.get("/table/api/state")).json()
        assert after["totals"]["hands"] - before["totals"]["hands"] == 3
        assert after["presence"]["last"]["name"] == "your program \u201cexample\u201d"
        # Every move was a check, a call or a fold to nothing (a call-every-bet bot).
        kinds = {e["kind"] for h in (await browser.get("/table/api/state", params={"since": before["hand"]})).json()["history"]
                 for e in h["events"] if e["seat"] == "you"}
        assert kinds <= {"check", "call"}, kinds


@pytest.mark.parametrize("command", RUNS)
def test_an_onboarding_example_without_a_token_says_where_to_get_one(command, tmp_path):
    env = {k: v for k, v in os.environ.items() if k != "OBGL_TOKEN"}
    run = subprocess.run([*command, "1"], cwd=tmp_path, env=env, capture_output=True, text=True, timeout=60)
    assert run.returncode != 0 and "OBGL_TOKEN" in run.stderr and "obgl-token.txt" in run.stderr, run.stderr
