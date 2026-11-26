"""tables.py: each account's one table (made once, kept, reset, an old
account's newest session), whose table a request may name, who is at the
table, and the finished-hand cache. In-process, no network: a fake Go
table."""

import asyncio
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import FileStore, MemoryStore
from table_client import TableError
from tables import HANDS_KEPT, Party, Tables

A, B = "github:4242", "google:1098765"


class FakeTable:
    """The Go table's new and state, as TableClient has them."""

    def __init__(self):
        self.sessions = {}
        self.made = 0

    def new(self, seat="alternate", opponent=None):
        self.made += 1
        sid = f"s{self.made}"
        self.sessions[sid] = {"session": sid, "hand": 1, "step": 0, "over": False, "seatMode": seat, "opponent": opponent or "turtle"}
        return dict(self.sessions[sid])

    def state(self, session):
        if session not in self.sessions:
            raise TableError(404, "no such session")
        return dict(self.sessions[session])


def test_one_table_per_account_made_once_and_kept_across_a_restart(tmp_path):
    path, fake = str(tmp_path / "oauth.json"), FakeTable()

    async def first():
        t = Tables(FileStore(path), fake)
        # Two requests at once for an account with no table yet: one table.
        (a1, _), (a2, _) = await asyncio.gather(t.ensure(A), t.ensure(A))
        b, _ = await t.ensure(B)
        assert a1 == a2 and a1 != b and fake.made == 2
        return a1

    a = asyncio.run(first())

    async def again():
        t = Tables(FileStore(path), fake)  # a fresh process: the file reopened
        assert (await t.ensure(A))[0] == a and fake.made == 2
        assert await t.current(A) == a

    asyncio.run(again())


def test_an_old_accounts_newest_session_is_its_table_and_a_lost_one_is_replaced():
    async def go():
        fake = FakeTable()
        store = MemoryStore()
        old1, old2 = fake.new()["session"], fake.new()["session"]
        await store.put("account-tables", A, {"sessions": [old2, old1]})
        t = Tables(store, fake)
        assert (await t.ensure(A))[0] == old2 and fake.made == 2
        del fake.sessions[old2]  # the Go table restarted and did not keep it
        new, state = await t.ensure(A)
        assert new not in (old1, old2) and state["hand"] == 1
        assert (await store.get("account-tables", A))["sessions"][:2] == [new, old2]
        assert (await store.get("table-sessions", new))["owner"] == A

    asyncio.run(go())


def test_reset_makes_a_new_table_and_only_the_current_one_can_be_named():
    async def go():
        t = Tables(MemoryStore(), FakeTable())
        with pytest.raises(LookupError):
            await t.resolve(A, None)  # no table yet
        first, _ = await t.ensure(A)
        assert await t.resolve(A, None) == first and await t.resolve(A, "") == first and await t.resolve(A, first) == first
        second, state = await t.reset(A, seat="bb", opponent="ox")
        assert second != first and state["seatMode"] == "bb" and state["opponent"] == "ox"
        assert await t.resolve(A, None) == second
        b, _ = await t.ensure(B)
        for other in (first, b, "nope", second + "x"):  # an old one of A's, B's, made up: all alike
            with pytest.raises(LookupError):
                await t.resolve(A, other)

    asyncio.run(go())


def test_presence_is_in_memory_only_and_says_who_moved():
    async def go():
        clock = [1000.0]
        store = MemoryStore()
        t = Tables(store, FakeTable(), clock=lambda: clock[0])
        sid, _ = await t.ensure(A)
        before = {kind: dict(records) for kind, records in store._data.items()}
        assert t.presence(sid) == {"last": None, "others": [], "hands": {}}
        bot = Party.program("ab12cd34", "my bot")
        for hand in range(1, HANDS_KEPT + 3):
            t.moved(sid, bot, {"hand": hand, "step": 0, "over": False})
            clock[0] += 0.1
            t.moved(sid, bot, {"hand": hand, "step": 1, "over": True})  # dealing the next: not a move in that hand
        clock[0] += 1
        t.seen(sid, Party.agent("conn-1"))
        clock[0] += 2
        t.moved(sid, Party.browser(), {"hand": HANDS_KEPT + 2, "step": 1, "over": False})
        p = t.presence(sid)
        assert p["last"] == {"kind": "browser", "name": "you", "ago": 0, "hand": HANDS_KEPT + 2}
        assert [(o["kind"], o["name"]) for o in p["others"]] == [("agent", "your agent"), ("program", "your program \u201cmy bot\u201d")]
        assert [o["ago"] for o in p["others"]] == [2, 3]
        assert len(p["hands"]) == HANDS_KEPT and "1" not in p["hands"]
        assert p["hands"][str(HANDS_KEPT + 2)] == ["your program \u201cmy bot\u201d", "you"]
        assert t.last_mover(sid) == "you"
        # Not one store write for any of it.
        assert {kind: dict(records) for kind, records in store._data.items()} == before

    asyncio.run(go())


def test_finished_hands_are_kept_for_a_page_that_polled_past_them():
    t = Tables(MemoryStore(), FakeTable())
    t.observe("s", {"hand": 1, "over": False})
    assert t.final_state("s", 1) is None
    for hand in (1, 2, 3):
        t.observe("s", {"hand": hand, "over": True, "net": hand})
    assert t.final_state("s", 1) is None  # the latest two only
    assert t.final_state("s", 3)["net"] == 3 and t.final_state("s", 2)["net"] == 2
