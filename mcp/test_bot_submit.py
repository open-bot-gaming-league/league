"""Bot submission over HTTP (bot_web.py's routes on the table's API, a
local file store, the real wasm check: poker/arena/cmd/botcheck), and the
SDK's client for it, `go run ./sdk/cmd/bot`, as a coach's agent runs it.
Skips without `go` on PATH."""

import hashlib
import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio.to_thread
import httpx2
import pytest

from bot_files import LocalFiles
from bots import Bots, GoBotCheck, Limits
from conftest import USER_A, USER_B

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ENGINE = os.path.join(REPO, "poker", "engine")
ARENA = os.path.join(REPO, "poker", "arena")
EMPTY = b"\0asm\x01\0\0\0"  # the empty module: valid WebAssembly, but not a bot

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="no go on PATH")


def _build(cwd: str, pkg: str, out: str) -> str:
    built = subprocess.run(["go", "build", "-o", out, pkg], cwd=cwd, capture_output=True, text=True, timeout=600)
    assert built.returncode == 0, built.stderr
    return out


@pytest.fixture(scope="module")
def tools(tmp_path_factory):
    d = tmp_path_factory.mktemp("tools")
    return {"botcheck": _build(ARENA, "./cmd/botcheck", str(d / "botcheck")),
            "bot": _build(ENGINE, "./sdk/cmd/bot", str(d / "bot"))}


@pytest.fixture(scope="module")
def wasm(tmp_path_factory):
    """The SDK's example built as the SDK README says: a bot botcheck passes."""
    out = tmp_path_factory.mktemp("wasm") / "bot.wasm"
    built = subprocess.run(["go", "build", "-buildmode=c-shared", "-o", str(out), "./sdk/example"], cwd=ENGINE,
                           env={**os.environ, "GOOS": "wasip1", "GOARCH": "wasm"}, capture_output=True, text=True, timeout=600)
    assert built.returncode == 0, built.stderr
    return out.read_bytes()


@pytest.fixture
def league(bot_site, tools, tmp_path):
    def bots(base, store):
        return Bots(store, LocalFiles(str(tmp_path / "store"), base), GoBotCheck(tools["botcheck"]),
                    Limits(wasm_bytes=32 << 20, data_bytes=8 << 20))
    with bot_site(bots) as site:
        yield site


async def _token(site, user=USER_A) -> str:
    async with site.browser(user) as browser:
        return (await browser.post("/table/api/token", json={"label": "bot test"})).json()["token"]


def _entry(data: bytes) -> dict:
    return {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}


async def _upload(http, session: str, data: bytes) -> None:
    done = await http.put(session, content=data, headers={"Content-Range": f"bytes 0-{len(data) - 1}/{len(data)}"})
    assert done.status_code == 200, done.text


@pytest.mark.asyncio
async def test_the_api_takes_a_bot_and_refuses_what_it_should(league, wasm):
    token = await _token(league)
    auth = {"Authorization": f"Bearer {token}"}
    async with httpx2.AsyncClient(base_url=league.base, timeout=60) as http:
        assert (await http.get("/table/api/bots")).status_code == 401
        # Over a cap: refused before any upload, every problem named.
        r = await http.post("/table/api/bots/submissions", headers=auth,
                            json={"name": "mybot", "wasm": {"size": 64 << 20, "sha256": "0" * 64}, "data": []})
        assert r.status_code == 400 and r.json()["problems"] == ["wasm: 67,108,864 bytes is over the 33,554,432-byte cap"]
        # Bytes that are not the ones declared.
        r = await http.post("/table/api/bots/submissions", headers=auth,
                            json={"name": "mybot", "wasm": _entry(wasm), "data": [{"path": "d.bin", **_entry(b"declared")}]})
        assert r.status_code == 200, r.text
        sub = r.json()
        for u in sub["uploads"]:
            await _upload(http, u["session"], wasm if u["sha256"] == _entry(wasm)["sha256"] else b"uploaded")
        r = await http.post(f"/table/api/bots/submissions/{sub['id']}/finalize", headers=auth)
        assert r.status_code == 422 and r.json()["problems"] == [
            "data file 'd.bin': its sha256 is not the one declared (the file changed, or the upload was damaged)"], r.text
        # Not a module botcheck can compile, and a module that is not a bot.
        for refused, reason in [(EMPTY + b"\x01\xff", "wasm: not a valid WebAssembly module"),
                                (EMPTY, "wasm: the module does not export _initialize")]:
            r = await http.post("/table/api/bots/submissions", headers=auth, json={"name": "mybot", "wasm": _entry(refused)})
            sub = r.json()
            await _upload(http, sub["uploads"][0]["session"], refused)
            r = await http.post(f"/table/api/bots/submissions/{sub['id']}/finalize", headers=auth)
            assert r.status_code == 422 and r.json()["problems"][0].startswith(reason), r.text
        # A good one.
        r = await http.post("/table/api/bots/submissions", headers=auth, json={"name": "mybot", "wasm": _entry(wasm)})
        sub = r.json()
        await _upload(http, sub["uploads"][0]["session"], wasm)
        assert (await http.get(f"/table/api/bots/submissions/{sub['id']}", headers=auth)).json()["uploads"][0]["done"]
        r = await http.post(f"/table/api/bots/submissions/{sub['id']}/finalize", headers=auth)
        assert r.status_code == 200 and r.json()["bot"]["version"] == 1, r.text
        # The hour's one is used.
        r = await http.post("/table/api/bots/submissions", headers=auth, json={"name": "mybot", "wasm": _entry(wasm)})
        assert r.status_code == 429 and "an hour" in r.json()["error"] and "retry_at" in r.json()
        assert "retry-after" not in r.headers
        listed = (await http.get("/table/api/bots", headers=auth)).json()
        assert [(b["name"], b["version"]) for b in listed["bots"]] == [("mybot", 1)] and listed["open"] is None
        # Another account sees none of it.
        other = {"Authorization": f"Bearer {await _token(league, USER_B)}"}
        assert (await http.get("/table/api/bots", headers=other)).json() == {"bots": [], "open": None}
        assert (await http.get(f"/table/api/bots/submissions/{sub['id']}", headers=other)).status_code == 404
        # A session's address is the permission for its one file: another address is no session.
        r = await http.post("/table/api/bots/submissions", headers=other, json={"name": "b", "wasm": _entry(wasm)})
        session = r.json()["uploads"][0]["session"]
        assert session.startswith(f"{league.base}/bots/upload/")
        wrong = session[:-1] + ("0" if session[-1] != "0" else "1")
        assert (await http.put(wrong, headers={"Content-Range": "bytes */8"})).status_code == 404


@pytest.mark.asyncio
async def test_the_sdk_submits_and_lists(league, tools, wasm, tmp_path):
    token = await _token(league)
    work = tmp_path / "work"
    (work / "data" / "tables").mkdir(parents=True)
    (work / "bot.wasm").write_bytes(wasm)
    (work / "data" / "tables" / "t.bin").write_bytes(os.urandom(3 << 20))
    (work / "data" / "small.txt").write_bytes(b"hello")
    env = {**os.environ, "OBGL_TABLE": f"{league.base}/table/", "OBGL_TOKEN": token}

    def bot(*args):
        return subprocess.run([tools["bot"], *args], cwd=work, env=env, capture_output=True, text=True, timeout=120)

    done = await anyio.to_thread.run_sync(lambda: bot("list"))
    assert done.returncode == 0 and done.stdout.startswith("no bots submitted yet"), done.stdout + done.stderr
    done = await anyio.to_thread.run_sync(lambda: bot("submit", "-name", "mybot", "-wasm", "bot.wasm", "-data", "data"))
    assert done.returncode == 0, done.stdout + done.stderr
    lines = done.stdout.splitlines()
    assert re.fullmatch(r"bot mybot: wasm bot\.wasm [\d.]+ MB; data 2 files 3\.1 MB", lines[0]), lines
    assert any(re.search(r"3 files to upload, [\d.]+ MB$", line) for line in lines), lines
    assert lines[-1].startswith("submitted: mybot v1  ") and lines[-1].endswith("data 2 files 3.1 MB"), lines
    done = await anyio.to_thread.run_sync(lambda: bot("list"))
    assert done.returncode == 0 and done.stdout.splitlines() == [lines[-1].removeprefix("submitted: ")], done.stdout
    done = await anyio.to_thread.run_sync(lambda: bot("submit", "-name", "mybot", "-wasm", "bot.wasm", "-data", "data"))
    assert done.returncode == 1 and "429" in done.stderr and "You can submit one bot an hour" in done.stderr, done.stdout + done.stderr
    del env["OBGL_TOKEN"]
    done = await anyio.to_thread.run_sync(lambda: bot("list"))
    assert done.returncode == 1 and "no program token" in done.stderr, done.stderr
