"""The Go SDK's table client (poker/engine/sdk, sdk.Table) against this
server: a program token made by the signed-in page, and the SDK's example
bot (`go run ./sdk/example -table`) playing the account's table with it,
as a coach's program would. Skips without `go` on PATH or the live table.
"""

import os
import shutil
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio.to_thread
import pytest

from conftest import USER_A
from table_client import TableClient

ENGINE = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "poker", "engine")


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


@pytest.mark.asyncio
@pytest.mark.skipif(shutil.which("go") is None, reason="no go on PATH")
@pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")
async def test_the_sdk_plays_the_accounts_table_with_a_program_token(site, tmp_path):
    bin_path = str(tmp_path / "example")
    built = await anyio.to_thread.run_sync(lambda: subprocess.run(
        ["go", "build", "-o", bin_path, "./sdk/example"], cwd=ENGINE, capture_output=True, text=True, timeout=300))
    assert built.returncode == 0, built.stderr
    async with site.browser(USER_A) as browser:
        before = (await browser.get("/table/api/state")).json()
        token = (await browser.post("/table/api/token", json={"label": "sdk test"})).json()["token"]

        def run(token):
            env = {**os.environ, "OBGL_TABLE": f"{site.base}/table/", "OBGL_TOKEN": token}
            return subprocess.run([bin_path, "-table", "-hands", "3"], env=env, capture_output=True, text=True, timeout=120)

        played = await anyio.to_thread.run_sync(lambda: run(token))
        assert played.returncode == 0, played.stdout + played.stderr
        lines = played.stdout.splitlines()
        assert sum(line.startswith("hand ") for line in lines) == 3, played.stdout
        assert f"session {before['session']}" in lines[-1], lines[-1]
        after = (await browser.get("/table/api/state")).json()
        assert after["session"] == before["session"] and after["totals"]["hands"] == before["totals"]["hands"] + 3
        assert after["presence"]["last"]["name"] == "your program \u201csdk test\u201d"
        # Without a token the SDK says what to do; with a revoked one, the same.
        refused = await anyio.to_thread.run_sync(lambda: run(""))
        assert refused.returncode != 0 and "program token" in refused.stderr, refused.stderr
