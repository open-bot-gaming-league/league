"""tables.py: ownership, watch tokens, control and its switch log, the
per-account list, and the finished-hand cache. In-process, no network."""

import asyncio
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import FileStore, MemoryStore
from tables import NotInControl, Party, TableSessions

A, B = "github:4242", "google:1098765"


def test_table_session_owner_survives_restart_and_is_owner_only(tmp_path):
    path = str(tmp_path / "oauth.json")

    async def record():
        t = TableSessions(FileStore(path))
        await t.create("t-a1", A, Party.agent("c1"))
        await t.create("t-b", B, Party.agent("c2"))
        await t.create("t-a2", A, Party.browser())

    asyncio.run(record())

    async def read():
        t = TableSessions(FileStore(path))  # a fresh process's view: the file reopened
        assert (await t.table_session_for("t-a1", A))["owner"] == A
        assert (await t.table_session_for("t-b", B))["owner"] == B
        # Another account reads nothing.
        assert await t.table_session_for("t-a1", B) is None
        assert await t.table_session_for("t-b", A) is None
        # An account matches only its own exact id: no prefix or provider-only match.
        assert await t.table_session_for("t-a1", "github:") is None
        assert await t.table_session_for("t-a1", "github:424") is None
        assert await t.table_session_for("nope", A) is None

    asyncio.run(read())


def test_watch_token_is_not_the_session_id_and_is_owner_only():
    async def go():
        t = TableSessions(MemoryStore())
        rec = await t.create("s1", A, Party.agent("c1"))
        token = rec["watch"]
        assert token and token != "s1" and len(token) >= 32
        assert await t.by_watch(token, A) == ("s1", rec)
        assert await t.by_watch(token, B) is None
        assert await t.by_watch("s1", A) is None  # the session id is not a watch token
        assert await t.by_watch("", A) is None
        # The token is filed by its hash, not in the clear, in the index.
        assert token not in t.store._data["watch-tokens"]

    asyncio.run(go())


def test_one_controller_at_a_time_and_every_switch_is_logged():
    async def go():
        clock = iter(range(1000, 2000))
        t = TableSessions(MemoryStore(), clock=lambda: next(clock))
        await t.create("s1", A, Party.agent("c1"))
        await t.require_control("s1", A, Party.agent("c1"))
        for party in (Party.browser(), Party.agent("c2")):
            with pytest.raises(NotInControl):
                await t.require_control("s1", A, party)
        with pytest.raises(LookupError):
            await t.require_control("s1", B, Party.agent("c1"))  # not B's: as if absent

        await t.switch("s1", A, Party.browser(), hand=2, step=3)  # take over, mid-hand
        await t.require_control("s1", A, Party.browser())
        with pytest.raises(NotInControl):
            await t.require_control("s1", A, Party.agent("c1"))
        await t.switch("s1", A, Party.browser(), hand=2, step=3)  # already: not logged again
        await t.switch("s1", A, Party("agent", None), hand=2, step=4, by=Party.browser())  # tag in
        for party in (Party.browser(), Party.agent("c1")):
            with pytest.raises(NotInControl):
                await t.require_control("s1", A, party)  # unclaimed: nobody acts
        rec = await t.switch("s1", A, Party.agent("c2"), hand=2, step=4)  # claimed
        await t.require_control("s1", A, Party.agent("c2"))
        assert [(s["hand"], s["step"], s["from"], s["to"], s["by"]) for s in rec["switches"]] == [
            (2, 3, "agent", "browser", "browser"),
            (2, 4, "browser", "unclaimed", "browser"),
            (2, 4, "unclaimed", "agent", "agent"),
        ]
        assert all(isinstance(s["at"], int) for s in rec["switches"])
        with pytest.raises(LookupError):
            await t.switch("s1", B, Party.browser(), hand=2, step=4)

    asyncio.run(go())


def test_list_is_per_account_newest_first_and_bounded():
    async def go():
        t = TableSessions(MemoryStore())
        for i in range(25):
            await t.create(f"a{i}", A, Party.browser())
        await t.create("b0", B, Party.browser())
        listed = [sid for sid, _ in await t.list_for(A)]
        assert listed == [f"a{i}" for i in range(24, 4, -1)]
        assert [sid for sid, _ in await t.list_for(B)] == ["b0"]
        assert await t.list_for("github:9999") == []

    asyncio.run(go())


def test_finished_hands_are_kept_for_watchers():
    t = TableSessions(MemoryStore())
    t.observe("s1", {"hand": 1, "step": 2, "over": False})
    assert t.final_state("s1", 1) is None
    for hand in (1, 2, 3):
        t.observe("s1", {"hand": hand, "step": 5, "over": True})
    assert t.final_state("s1", 1) is None  # the latest two only
    assert t.final_state("s1", 2)["hand"] == 2 and t.final_state("s1", 3)["hand"] == 3
    assert t.final_state("s2", 3) is None
