"""Checking submissions apart from the request (finalize.py, bots.py's
FinalizeQueue): a finalize queues the submission, state checking; the
runner files the version and queues its ranked table, or rejects it; one
runner at a time under a lease, a stopped one's submission checked again;
the job started when one is queued, and again when a coach asks after one
no runner holds. Fake starters and a local store: no GCP."""

import hashlib
import json
import logging
import os
import stat
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio
import anyio.to_thread
import pytest

from bot_files import LocalFiles, bot_key
from bots import FINALIZE_QUEUE, Bots, FinalizeQueue, Limits
from finalize import MAX_STARTS, Finalizer
from jobs import START_GRACE
from oauth.store import FileStore, MemoryStore
from ranked import QUEUE, RankedTables, table_id
from test_bots import ALICE, BOB, WASM, Clock, accept, by_sha, manifest, upload_all

ME_DIR = os.path.dirname(os.path.abspath(__file__))


class Starter:
    def __init__(self):
        self.starts, self.fail = 0, False

    def start(self):
        if self.fail:
            raise RuntimeError("jobs.run: HTTP 503 (a test)")
        self.starts += 1
        return f"execution {self.starts}"


def _league(tmp_path, check=accept, starter=None, store=None):
    clock, store = Clock(), store or MemoryStore()
    ranked = RankedTables(store, clock)
    bots = Bots(store, LocalFiles(str(tmp_path / "bots"), "http://league.test"), check, Limits(per_hour=0), clock,
                on_finalized=ranked.enqueue, queue=FinalizeQueue(store, clock, starter))
    return bots, ranked, clock


async def _asked(bots, account, wasm=WASM, name="mybot"):
    sub = await bots.create(account, manifest(name, wasm))
    upload_all(bots, sub, by_sha(wasm))
    asked = await bots.finalize(account, sub["id"])
    assert asked["state"] == "checking" and "bot" not in asked
    return sub["id"]


@pytest.mark.asyncio
async def test_a_finalize_is_checked_by_the_runner_and_files_the_version_and_its_table(tmp_path):
    bots, ranked, _ = _league(tmp_path)
    sid = await _asked(bots, ALICE)
    assert (await bots.status(ALICE, sid))["state"] == "checking"
    assert (await bots.list(ALICE))["bots"] == []  # nothing is a version until it is checked
    await Finalizer(bots).drain()
    done = await bots.status(ALICE, sid)
    assert done["state"] == "finalized" and done["bot"]["version"] == 1 and done["table"] == table_id(sid)
    assert "lease_until" not in await bots.store.get("bot-submissions", sid)
    assert (await bots.list(ALICE))["bots"] == [done["bot"]]
    assert await ranked.queue() == [table_id(sid)] and await bots.queue.submissions() == []


@pytest.mark.asyncio
async def test_a_rejected_submission_leaves_no_version_and_no_table(tmp_path):
    bots, ranked, _ = _league(tmp_path)
    sid = await _asked(bots, ALICE, wasm=WASM + b"\0\x07refuse")
    await Finalizer(bots).drain()
    done = await bots.status(ALICE, sid)
    assert done["state"] == "rejected" and done["problems"] == ["wasm: it exports no bot (a test module)"]
    assert (await bots.list(ALICE))["bots"] == [] and await ranked.queue() == []
    assert not os.path.exists(os.path.join(bots.files.root, "uploads", ALICE, sid))
    assert await bots.store.get("account-bots", ALICE) == {"versions": [], "open": None, "checking": None,
                                                           "asked": [1_800_000_000]}  # the hour's one, used
    assert (await bots.status(ALICE, await _asked(bots, ALICE)))["state"] == "checking"  # the account may go again


@pytest.mark.asyncio
async def test_the_runner_drains_the_queue_in_order(tmp_path):
    bots, ranked, _ = _league(tmp_path)
    first, second = await _asked(bots, ALICE), await _asked(bots, BOB)
    assert await bots.queue.submissions() == [first, second]
    await Finalizer(bots).drain()
    assert [(await bots.status(a, s))["state"] for a, s in ((ALICE, first), (BOB, second))] == ["finalized"] * 2
    assert await ranked.queue() == [table_id(first), table_id(second)]


@pytest.mark.asyncio
async def test_a_stopped_runners_submission_is_checked_again_and_failed_after_max_starts(tmp_path):
    bots, _, clock = _league(tmp_path)
    sid = await _asked(bots, ALICE)
    # A runner died holding it: its lease is live, so another leaves it.
    sub = await bots.store.get("bot-submissions", sid)
    await bots.store.put("bot-submissions", sid, {**sub, "lease_until": clock() + 120, "attempts": 1})
    await Finalizer(bots).drain()
    assert (await bots.status(ALICE, sid))["state"] == "checking"
    clock.t += 121  # the lease ran out: the next runner checks it again
    await Finalizer(bots).drain()
    assert (await bots.status(ALICE, sid))["state"] == "finalized"

    other = await _asked(bots, BOB)
    sub = await bots.store.get("bot-submissions", other)
    await bots.store.put("bot-submissions", other, {**sub, "lease_until": clock() - 1, "attempts": MAX_STARTS})
    await Finalizer(bots).drain()
    done = await bots.status(BOB, other)
    assert done["state"] == "rejected" and "interrupted 3 times" in done["problems"][0]
    assert (await bots.list(BOB))["bots"] == []


@pytest.mark.asyncio
async def test_a_runner_stopped_mid_check_leaves_it_for_the_next_start(tmp_path):
    bots, _, _ = _league(tmp_path)
    sid = await _asked(bots, ALICE)
    checking = anyio.Event()
    real = bots.check

    async def stuck(s):  # a check the runner is stopped in (the job's task shut down)
        checking.set()
        await anyio.sleep_forever()

    bots.check = stuck
    async with anyio.create_task_group() as tg:
        tg.start_soon(Finalizer(bots).drain)
        await checking.wait()
        tg.cancel_scope.cancel()
    sub = await bots.store.get("bot-submissions", sid)
    assert sub["state"] == "checking" and sub["lease_until"] == 0 and sub["attempts"] == 1  # free now; the start counted
    bots.check = real
    await Finalizer(bots).drain()
    assert (await bots.status(ALICE, sid))["state"] == "finalized"


@pytest.mark.asyncio
async def test_a_version_filed_again_queues_one_table(tmp_path):
    bots, ranked, _ = _league(tmp_path)
    sid = await _asked(bots, ALICE)
    await Finalizer(bots).drain()
    version = (await bots.status(ALICE, sid))["bot"]
    again = await ranked.enqueue(ALICE, version)  # as a runner stopped after the table, before noting it
    assert again["id"] == table_id(sid)
    assert await ranked.queue() == [table_id(sid)]
    assert (await bots.store.get("account-ranked", ALICE))["tables"] == [table_id(sid)]


@pytest.mark.asyncio
async def test_the_job_is_started_when_one_is_queued_and_when_a_coach_asks_after_one(tmp_path, caplog):
    starter = Starter()
    bots, _, clock = _league(tmp_path, starter=starter)
    sid = await _asked(bots, ALICE)
    assert starter.starts == 1
    await _asked(bots, BOB)
    assert starter.starts == 1  # started a moment ago: time for it to take the head
    await bots.status(ALICE, sid)
    assert starter.starts == 1
    clock.t += START_GRACE + 1  # it never came: the coach's next look starts another
    await bots.status(ALICE, sid)
    assert starter.starts == 2

    # A runner holds the head: nothing is started, and its taking the head
    # clears the last start, so the next queued after it starts at once.
    sub = await bots.store.get("bot-submissions", sid)
    await bots.store.put("bot-submissions", sid, {**sub, "lease_until": clock() + 120, "attempts": 1})
    await bots.queue.taken()
    clock.t += 1
    await bots.status(ALICE, sid)
    assert starter.starts == 2 and "started_at" not in await bots.store.get(*FINALIZE_QUEUE)

    # A start that fails: the finalize still answers, and the next look tries again.
    clock.t += 121
    starter.fail = True
    with caplog.at_level(logging.ERROR, logger="obgl.bots"):
        await bots.status(ALICE, sid)
    assert "did not start" in caplog.text and "HTTP 503" in caplog.text


@pytest.mark.asyncio
async def test_no_starter_wakes_the_runner_in_this_process(tmp_path):
    bots, _, _ = _league(tmp_path)
    async with anyio.create_task_group() as tg:
        tg.start_soon(Finalizer(bots).serve, 3600)
        await anyio.sleep(0.05)
        sid = await _asked(bots, ALICE)
        with anyio.fail_after(10):
            while (await bots.status(ALICE, sid))["state"] == "checking":
                await anyio.sleep(0.01)
        tg.cancel_scope.cancel()
    assert (await bots.status(ALICE, sid))["state"] == "finalized"


def test_the_job_command_checks_the_queue_and_exits(tmp_path):
    """`python finalize.py run` as the job runs it, on a file store and a local bot store."""
    auth = str(tmp_path / "auth.json")
    bots, _, _ = _league(tmp_path, store=FileStore(auth))
    sid = anyio.run(_asked, bots, ALICE)
    check = tmp_path / "botcheck"
    check.write_text("#!/bin/sh\ncat > /dev/null\nexit 0\n")
    check.chmod(check.stat().st_mode | stat.S_IXUSR)
    env = {k: v for k, v in os.environ.items() if not k.startswith("OBGL_")}
    env.update(OBGL_AUTH_STORE="file", OBGL_AUTH_FILE=auth, OBGL_BOT_STORE=f"local:{tmp_path / 'bots'}",
               OBGL_BOTCHECK=str(check))
    done = subprocess.run([sys.executable, os.path.join(ME_DIR, "finalize.py"), "run"], env=env, capture_output=True,
                          text=True, timeout=60)
    assert done.returncode == 0, done.stderr
    store = FileStore(auth)
    sub = anyio.run(store.get, "bot-submissions", sid)
    assert sub["state"] == "finalized" and sub["table"] == table_id(sid), done.stderr
    assert anyio.run(store.get, *FINALIZE_QUEUE)["submissions"] == []
    assert anyio.run(store.get, "ranked-queue", QUEUE)["tables"] == [table_id(sid)]
    assert os.path.exists(os.path.join(tmp_path / "bots", bot_key(ALICE, hashlib.sha256(WASM).hexdigest())))
