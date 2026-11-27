"""Ranked tables (ranked.py): a finalized bot version is queued, played by
the runner one table at a time (the real rankedtable, every seat wasm, a few
hands a pair), its record filed and listed for its account; the page and
table_results read it from the real Go table (cmd/table -records). Also: a
bot that breaks every limit still gets a finished table that shows its
faults, and a runner stopped mid-queue loses no table (the queue is in the
auth store: a file here, Firestore when an emulator is up).
Skips without `go` on PATH."""

import hashlib
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio
import anyio.to_thread
import httpx2
import pytest

import oauth_testing as ot
from bot_files import LocalFiles
from bots import Bots, GoBotCheck, Limits
from conftest import USER_A, USER_B, Wiring
from finalize import Finalizer
from oauth.store import FileStore
from badges import earned
from ranked import MAX_STARTS, QUEUE, RankedTables, RecordDir, Runner, TableSettings, runner_from_env

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ENGINE = os.path.join(REPO, "poker", "engine")
ARENA = os.path.join(REPO, "poker", "arena")
TINY = TableSettings(pool=("dove", "clam", "shark"), seats=3, hands=20, parallel=2)  # 4 players, 6 pairs

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="no go on PATH")


def _go(cwd, *args, env=None):
    done = subprocess.run(["go", *args], cwd=cwd, capture_output=True, text=True, timeout=900, env=env)
    assert done.returncode == 0, done.stderr
    return done


def _wasm(cwd, pkg, out):
    _go(cwd, "build", "-buildmode=c-shared", "-o", str(out), pkg, env={**os.environ, "GOOS": "wasip1", "GOARCH": "wasm"})
    return out


@pytest.fixture(scope="module")
def built(tmp_path_factory):
    """The binaries, the house guests prebuilt (as the image has them), and
    two bots: the SDK's example, and a guest that loops forever in Act."""
    d = tmp_path_factory.mktemp("built")
    tools = {name: str(d / name) for name in ("botcheck", "rankedtable", "bot", "table")}
    _go(ARENA, "build", "-o", tools["botcheck"], "./cmd/botcheck")
    _go(ARENA, "build", "-o", tools["rankedtable"], "./cmd/rankedtable")
    _go(ENGINE, "build", "-o", tools["bot"], "./sdk/cmd/bot")
    _go(ENGINE, "build", "-o", tools["table"], "./cmd/table")
    guests = d / "guests"
    guests.mkdir()
    for g in TINY.pool:
        _wasm(ARENA, f"./guests/{g}", guests / f"{g}.wasm")
    return {**tools, "guests": str(guests),
            "example": _wasm(ENGINE, "./sdk/example", d / "example.wasm").read_bytes(),
            "loop": _wasm(ARENA, "./testdata/guests/loop", d / "loop.wasm").read_bytes()}


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def go_table(built, tmp_path_factory):
    """The Go table with -records: (its URL, the records directory)."""
    d = tmp_path_factory.mktemp("table")
    _go(ENGINE, "run", "./cmd/train", "-game", "holdem", "-config", "configs/table_50bb.json", "-abs", "category",
        "-iters", "1", "-out", str(d))
    (d / "bots.json").write_text(json.dumps([{"id": "u", "name": "Uniform", "trained": "one iteration (test)",
                                              "abs": "category", "blueprint": "blueprint-1.bp"}]))
    records = d / "records"
    records.mkdir()
    port = _free_port()
    proc = subprocess.Popen([built["table"], "-config", os.path.join(ENGINE, "configs", "table_50bb.json"),
                             "-bots", str(d / "bots.json"), "-records", str(records), "-addr", f"127.0.0.1:{port}"],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    url = f"http://127.0.0.1:{port}/"
    try:
        deadline = time.monotonic() + 30
        while True:
            try:
                urllib.request.urlopen(url + "health", timeout=1)
                break
            except Exception:
                assert time.monotonic() < deadline and proc.poll() is None, "the Go table did not come up"
                time.sleep(0.2)
        yield url, str(records)
    finally:
        proc.terminate()
        proc.wait(timeout=10)


def _league(built, tmp_path, records, in_process):
    """The bots maker for bot_site: submissions on a local store, each
    finalized version queued, and (in_process) the runner in the server."""
    made = {}

    def wiring(base, store):
        files = LocalFiles(str(tmp_path / "store"), base)
        ranked = RankedTables(store)
        runner = Runner(ranked, built["rankedtable"], built["guests"], files.root, RecordDir(records), TINY, lease=30)
        made.update(ranked=ranked, runner=runner)
        bots = Bots(store, files, GoBotCheck(built["botcheck"]), Limits(data_bytes=8 << 20, per_hour=0),
                    on_finalized=ranked.enqueue)
        return Wiring(bots, ranked, runner if in_process else None)

    return wiring, made


async def _token(site, user=USER_A):
    async with site.browser(user) as browser:
        return (await browser.post("/table/api/token", json={"label": "ranked test"})).json()["token"]


def _entry(data):
    return {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}


async def _submit(http, auth, name, wasm):
    sub = (await http.post("/table/api/bots/submissions", headers=auth, json={"name": name, "wasm": _entry(wasm)})).json()
    for u in sub["uploads"]:
        r = await http.put(u["session"], content=wasm, headers={"Content-Range": f"bytes 0-{len(wasm) - 1}/{len(wasm)}"})
        assert r.status_code == 200, r.text
    r = await http.post(f"/table/api/bots/submissions/{sub['id']}/finalize", headers=auth)
    assert r.status_code == 200 and r.json()["state"] == "checking", r.text
    with anyio.fail_after(120):  # checked in the server (finalize.py's runner)
        while r.json()["state"] == "checking":
            await anyio.sleep(0.2)
            r = await http.get(f"/table/api/bots/submissions/{sub['id']}", headers=auth)
    assert r.json()["state"] == "finalized", r.text
    return r.json()


async def _until_finished(http, auth, timeout=180):
    deadline = time.monotonic() + timeout
    while True:
        tables = (await http.get("/table/api/tables", headers=auth)).json()["tables"]
        if tables and tables[0]["state"] in ("done", "failed"):
            return tables
        assert time.monotonic() < deadline, tables
        await anyio.sleep(0.5)


@pytest.mark.asyncio
async def test_submit_queue_play_list_and_read(bot_site, built, go_table, tmp_path):
    """The whole of it on the local stack's wiring: the SDK submits, the
    server queues and (in-process) plays the table, the list links the
    result, the page serves it, and table_results with no id reads it."""
    table_url, records = go_table
    wiring, _ = _league(built, tmp_path, records, in_process=True)
    with bot_site(wiring, table_url=table_url) as site:
        token = await _token(site)
        auth = {"Authorization": f"Bearer {token}"}
        work = tmp_path / "work"
        (work / "data" / "tables").mkdir(parents=True)
        (work / "bot.wasm").write_bytes(built["example"])
        (work / "data" / "tables" / "t.bin").write_bytes(b"x" * 1000)
        env = {**os.environ, "OBGL_TABLE": f"{site.base}/table/", "OBGL_TOKEN": token}
        done = await anyio.to_thread.run_sync(lambda: subprocess.run(
            [built["bot"], "submit", "-name", "mybot", "-wasm", "bot.wasm", "-data", "data"],
            cwd=work, env=env, capture_output=True, text=True, timeout=120))
        assert done.returncode == 0, done.stdout + done.stderr
        queued = [line for line in done.stdout.splitlines() if line.startswith("ranked table ")]
        # The table as the check left it: the runner in the server may have taken it already.
        assert len(queued) == 1 and re.search(r" (queued \(number 1\)|playing now|played already): ", queued[0]), done.stdout
        table_id = queued[0].split()[2]

        async with httpx2.AsyncClient(base_url=site.base, timeout=60) as http:
            tables = await _until_finished(http, auth)
            assert len(tables) == 1, tables
            t = tables[0]
            assert t["id"] == table_id and t["state"] == "done", t
            assert (t["bot"], t["version"], t["result"], t["url"]) == ("mybot", 1, table_id, f"/table/results/{table_id}")
            assert t["players"] == 4 and 1 <= t["place"] <= 4 and t["started_at"] <= t["finished_at"]
            # Its badges: as many as its place reached (at four players, a win and a top 3).
            names = [r.name for r in earned(t["place"], t["players"])]
            assert t["badges"] == names
            badges = (await http.get("/table/api/badges", headers=auth)).json()
            assert [b["name"] for b in badges["badges"]] == names and {b["table"] for b in badges["badges"]} <= {table_id}
            assert badges["points"] == sum(r.points for r in earned(t["place"], t["players"]))
            # Only the account's own: another sees none.
            other = {"Authorization": f"Bearer {await _token(site, USER_B)}"}
            assert (await http.get("/table/api/tables", headers=other)).json() == {"tables": []}
            assert (await http.get("/table/api/tables")).status_code == 401
            assert (await http.get("/table/api/info", headers=auth)).json()["league"]["ranked"] is True
            page = await http.get(t["url"])
            assert page.status_code == 200 and "mybot v1 placed" in page.text, page.text[:500]
            assert 'id="faults"' not in page.text

        with open(os.path.join(records, table_id + ".json")) as f:
            rec = json.load(f)
        owner = rec["seats"][0]["owner"]
        assert owner["account"] == "github:1001" and (owner["bot"], owner["version"]) == ("mybot", 1) and owner["submission"]
        assert rec["seats"][0]["name"] == "mybot v1" and " data sha256:" in rec["seats"][0]["version"]
        assert [s.get("owner") for s in rec["seats"][1:]] == [None] * 3 and rec["hands_per_pair"] == 20

        async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
            result = await mcp.call_tool("table_results", {})
            assert not result.is_error, result.content
            text = result.content[0].text
            assert text.startswith(f"Your newest ranked table: mybot v1 (table {table_id}).\n\nmybot v1 placed "), text
            report = text[:text.index(f"/table/results/{table_id}\n")]
            assert ("Badges from this table: " in text) == bool(names) and "Badges" not in report
        async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_B)) as mcp:
            text = (await mcp.call_tool("table_results", {})).content[0].text
            assert text.startswith("You have no ranked tables yet."), text


@pytest.mark.asyncio
async def test_a_bot_that_breaks_its_limits_still_gets_a_table(bot_site, built, go_table, tmp_path):
    table_url, records = go_table
    wiring, made = _league(built, tmp_path, records, in_process=False)
    with bot_site(wiring, table_url=table_url) as site:
        auth = {"Authorization": f"Bearer {await _token(site)}"}
        async with httpx2.AsyncClient(base_url=site.base, timeout=60) as http:
            answer = await _submit(http, auth, "spinner", built["loop"])
            assert answer["table"]["state"] == "queued" and answer["table"]["position"] == 1
            tables = (await http.get("/table/api/tables", headers=auth)).json()["tables"]
            assert [(t["state"], t["position"]) for t in tables] == [("queued", 1)]
            async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
                text = (await mcp.call_tool("table_results", {})).content[0].text
                assert text.startswith("Your newest ranked table, spinner v1, is queued, number 1 in the queue"), text
            await made["runner"].drain()  # no runner in the server: the job's way
            [t] = (await http.get("/table/api/tables", headers=auth)).json()["tables"]
            assert t["state"] == "done", t
            page = (await http.get(t["url"])).text
            assert 'id="faults"' in page and "in 3 of its 3 pairs" in page, page
        with open(os.path.join(records, t["id"] + ".json")) as f:
            usage = json.load(f)["usage"][0]
        assert len(usage["faults"]) == 3 and usage["forfeited"] > 0, usage
        async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
            text = (await mcp.call_tool("table_results", {"table_id": t["id"]})).content[0].text
        assert "The sandbox played for these bots" in text and "spinner v1: " in text, text


# ---- the queue across runner restarts (no HTTP) ------------------------------------


async def _version(bots, account, wasm):
    """A finalized version of `account`'s, uploaded through LocalFiles' protocol."""
    sub = await bots.create(account, {"name": "b", "wasm": _entry(wasm)})
    for u in sub["uploads"]:
        sid = u["session"].rsplit("/", 1)[1]
        status, _, error = bots.files.put(sid, f"bytes 0-{len(wasm) - 1}/{len(wasm)}", iter([wasm]))
        assert status == 200, error
    await bots.finalize(account, sub["id"])
    await Finalizer(bots).drain()
    return (await bots.status(account, sub["id"]))["table"]


@pytest.mark.asyncio
async def test_a_runner_stopped_mid_queue_loses_no_table(built, tmp_path):
    path, files = str(tmp_path / "auth.json"), LocalFiles(str(tmp_path / "store"), "http://league.test")
    records = tmp_path / "records"

    def opened():
        """One process's view: the store file loaded afresh, a runner on it."""
        store = FileStore(path)
        ranked = RankedTables(store)
        bots = Bots(store, files, lambda wasm: None, Limits(per_hour=0), on_finalized=ranked.enqueue)
        return store, bots, Runner(ranked, built["rankedtable"], built["guests"], files.root, RecordDir(str(records)), TINY)

    store, bots, runner = opened()
    first = await _version(bots, "github:1", built["example"])
    second = await _version(bots, "github:2", built["example"])
    assert (await store.get("ranked-queue", QUEUE))["tables"] == [first, second]

    # Stopped while the first plays (a server shutting down).
    async with anyio.create_task_group() as tg:
        tg.start_soon(runner.drain)
        while (await store.get("ranked-tables", first))["state"] != "running":
            await anyio.sleep(0.05)
        await anyio.sleep(0.3)  # rankedtable is under way
        tg.cancel_scope.cancel()
    t = await store.get("ranked-tables", first)
    assert (t["state"], t["attempts"], t["lease_until"]) == ("running", 1, 0), t
    assert not (records / f"{first}.json").exists()

    # The next process plays both, the first again.
    store, bots, runner = opened()
    await runner.drain()
    for tid, starts in ((first, 2), (second, 1)):
        t = await store.get("ranked-tables", tid)
        assert (t["state"], t["attempts"]) == ("done", starts) and (records / f"{tid}.json").exists(), t
    assert (await store.get("ranked-queue", QUEUE))["tables"] == []

    # A crash (no chance to say so): running, its lease run out. Played
    # again -- unless it has been started MAX_STARTS times already.
    third = await _version(bots, "github:3", built["example"])
    t = await store.get("ranked-tables", third)
    await store.put("ranked-tables", third, {**t, "state": "running", "attempts": MAX_STARTS, "lease_until": time.time() - 1})
    await runner.drain()
    t = await store.get("ranked-tables", third)
    assert t["state"] == "failed" and t["reason"] == f"the table was interrupted {MAX_STARTS} times without finishing", t

    # Another runner's (its lease is live): left alone.
    fourth = await _version(bots, "github:4", built["example"])
    t = await store.get("ranked-tables", fourth)
    await store.put("ranked-tables", fourth, {**t, "state": "running", "attempts": 1, "lease_until": time.time() + 600})
    assert await runner.step() is False
    await store.put("ranked-tables", fourth, t)

    # The runner's own failure (no rankedtable) is not the table's: queued again, the start not counted.
    broken = Runner(runner.tables, str(tmp_path / "no-such-binary"), built["guests"], files.root, RecordDir(str(records)), TINY)
    with pytest.raises(OSError):
        await broken.step()
    t = await store.get("ranked-tables", fourth)
    assert (t["state"], t["attempts"]) == ("queued", 0), t

    # Deleted while queued (the account was): dropped from the queue, nothing filed.
    await store.delete("ranked-tables", fourth)
    assert await runner.step() is True
    assert (await store.get("ranked-queue", QUEUE))["tables"] == [] and not (records / f"{fourth}.json").exists()


@pytest.mark.asyncio
async def test_a_version_whose_files_are_gone_fails_with_a_reason(built, tmp_path):
    store = FileStore(str(tmp_path / "auth.json"))
    files = LocalFiles(str(tmp_path / "store"), "http://league.test")
    ranked = RankedTables(store)
    bots = Bots(store, files, lambda wasm: None, Limits(per_hour=0), on_finalized=ranked.enqueue)
    tid = await _version(bots, "github:1", built["example"])
    os.remove(os.path.join(files.root, "bots", "github:1", hashlib.sha256(built["example"]).hexdigest()))
    await Runner(ranked, built["rankedtable"], built["guests"], files.root, RecordDir(str(tmp_path / "r")), TINY).drain()
    t = await store.get("ranked-tables", tid)
    assert (t["state"], t["reason"]) == ("failed", "the bot's files are not in the bot store"), t
    assert (await ranked.list("github:1"))["tables"][0]["reason"] == t["reason"]


def test_the_runner_from_the_environment(built, tmp_path):
    ranked = RankedTables(FileStore(str(tmp_path / "auth.json")))
    env = {"OBGL_RANKEDTABLE": built["rankedtable"], "OBGL_RANKED_GUESTS": built["guests"], "OBGL_RECORDS": str(tmp_path / "r"),
           "OBGL_BOT_STORE": f"local:{tmp_path / 'store'}", "OBGL_RANKED_HANDS": "2000", "OBGL_RANKED_POOL": "dove, ox"}
    runner = runner_from_env(env, ranked)
    assert runner.files_dir == str(tmp_path / "store") and runner.settings == TableSettings(pool=("dove", "ox"), hands=2000)
    assert runner_from_env({**env, "OBGL_BOT_STORE": "gcs:b", "OBGL_BOT_FILES_DIR": "/bots"}, ranked).files_dir == "/bots"
    for change, problem in [({"OBGL_RECORDS": ""}, "needs OBGL_RECORDS"),
                            ({"OBGL_BOT_STORE": "gcs:b"}, "needs OBGL_BOT_FILES_DIR"),
                            ({"OBGL_RANKEDTABLE": str(tmp_path / "none")}, "is not a program"),
                            ({"OBGL_RANKED_GUESTS": str(tmp_path / "none")}, "is not a directory")]:
        with pytest.raises(ValueError, match=problem):
            runner_from_env({**env, **change}, ranked)
