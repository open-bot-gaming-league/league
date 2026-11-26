"""programs.py: program tokens made, shown once, kept hashed, listed,
used, revoked, capped, and rate-limited. In-process, no network."""

import asyncio
import hashlib
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import MemoryStore
from programs import BURST, CACHE_TTL, LAST_USED_EVERY, MAX_TOKENS, PREFIX, RATE, ProgramTokens, RateLimited, TooMany

A, B = "github:4242", "google:1098765"


def test_a_token_is_shown_once_kept_hashed_and_names_its_account():
    async def go():
        store = MemoryStore()
        p = ProgramTokens(store, clock=lambda: 5000.0)
        made = await p.create(A, "  my   first  bot  ")
        token = made["token"]
        assert token.startswith(PREFIX) and len(token) > 40 and made["label"] == "my first bot"
        # Nowhere in the store in the clear: only its sha256, as a key.
        dump = repr(store._data)
        assert token not in dump and token[len(PREFIX):] not in dump
        assert hashlib.sha256(token.encode()).hexdigest() in store._data["program-tokens"]
        # Reading it back never gives the token.
        assert await p.get(A) == {"id": made["id"], "label": made["label"], "created_at": 5000, "last_used_at": None}
        assert await p.get(B) is None
        prog = await p.check(token)
        assert (prog.account, prog.id, prog.label) == (A, made["id"], made["label"])
        for bad in ("", "obgl_at_" + token[len(PREFIX):], token + "x", token[:-1], PREFIX):
            assert await p.check(bad) is None, bad

    asyncio.run(go())


def test_last_used_is_written_once_a_minute_at_most():
    async def go():
        clock = [1000.0]
        store = MemoryStore()
        p = ProgramTokens(store, clock=lambda: clock[0])
        made = await p.create(A, "bot")
        writes = []
        put = store.put

        async def counting_put(kind, key, value):
            writes.append(kind)
            await put(kind, key, value)

        store.put = counting_put
        for _ in range(500):  # a program playing as fast as its rate allows: 30 s of requests
            assert await p.check(made["token"])
            clock[0] += 0.06
        assert writes == ["program-usage"]
        assert (await p.get(A))["last_used_at"] == 1000
        clock[0] += LAST_USED_EVERY
        await p.check(made["token"])
        assert writes == ["program-usage"] * 2

    asyncio.run(go())


def test_revoke_is_immediate_and_only_for_the_owner():
    async def go():
        p = ProgramTokens(MemoryStore())
        mine, theirs = await p.create(A, "a"), await p.create(B, "b")
        assert await p.check(mine["token"])  # (writes a usage record)
        assert await p.revoke(A)
        assert await p.check(mine["token"]) is None and await p.get(A) is None
        assert not await p.revoke(A)
        assert await p.check(theirs["token"])  # B's is B's
        assert await p.store.get("program-usage", mine["id"]) is None

    asyncio.run(go())


def test_one_token_an_account_regenerated_or_revoked():
    async def go():
        p = ProgramTokens(MemoryStore())
        first = await p.create(A, "bot")
        with pytest.raises(TooMany):
            await p.create(A, "another")
        assert MAX_TOKENS == 1 and (await p.get(A))["id"] == first["id"]
        await p.create(B, "another account's")  # each account its own
        new = await p.regenerate(A)
        assert new["token"] != first["token"] and new["label"] == "bot"
        assert await p.check(first["token"]) is None and (await p.check(new["token"])).id == new["id"]
        assert (await p.get(A))["id"] == new["id"]
        assert len(p.store._data["program-tokens"]) == 2  # A's new one and B's: the old is gone
        assert await p.revoke(A)
        again = await p.create(A, "")
        assert again["label"] == "my bot"  # an empty label gets one
        assert (await p.regenerate(A, "renamed"))["label"] == "renamed"

    asyncio.run(go())


def test_the_rate_limit_lets_a_burst_through_then_the_rate():
    async def go():
        clock = [0.0]
        p = ProgramTokens(MemoryStore(), clock=lambda: clock[0])
        fast, slow = (await p.create(A, "fast"))["token"], (await p.create(B, "slow"))["token"]

        async def gets_through(token):
            try:
                return await p.check(token) is not None
            except RateLimited:
                return False

        assert [await gets_through(fast) for _ in range(int(BURST))] == [True] * int(BURST)
        with pytest.raises(RateLimited) as over:
            await p.check(fast)
        assert 0 < over.value.wait <= 1 / RATE
        assert await gets_through(slow)  # each token its own bucket
        clock[0] += 1  # a second later: RATE more
        assert sum([await gets_through(fast) for _ in range(int(RATE) + 5)]) == int(RATE)

    asyncio.run(go())


class CountingStore(MemoryStore):
    """A store that counts its reads, by kind."""

    def __init__(self):
        super().__init__()
        self.reads = {}

    async def get(self, kind, key):
        self.reads[kind] = self.reads.get(kind, 0) + 1
        return await super().get(kind, key)


def test_lookups_are_cached_and_unknown_tokens_are_limited_before_any_read():
    async def go():
        clock = [0.0]
        store = CountingStore()
        p = ProgramTokens(store, clock=lambda: clock[0])
        made = await p.create(A, "bot")
        store.reads.clear()
        unknown = PREFIX + "made-up-and-never-issued"
        # Requests with one unknown token: one read per CACHE_TTL, and over the rate, none.
        answered = 0
        for _ in range(200):
            try:
                assert await p.check(unknown) is None
                answered += 1
            except RateLimited:
                pass
            clock[0] += 0.01  # 100 a second: well over the rate
        assert store.reads.get("program-tokens") == 1 and answered < 200, (store.reads, answered)
        clock[0] += CACHE_TTL
        assert await p.check(unknown) is None and store.reads["program-tokens"] == 2
        # A real token: one read for many requests, then revoke works at once.
        store.reads.clear()
        for _ in range(30):
            assert (await p.check(made["token"])).id == made["id"]
            clock[0] += 0.1
        assert store.reads.get("program-tokens") == 1, store.reads
        assert await p.revoke(A)
        assert await p.check(made["token"]) is None

    asyncio.run(go())
