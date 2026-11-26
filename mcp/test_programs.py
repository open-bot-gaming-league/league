"""programs.py: program tokens made, shown once, kept hashed, listed,
used, revoked, capped, and rate-limited. In-process, no network."""

import asyncio
import hashlib
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import MemoryStore
from programs import BURST, LAST_USED_EVERY, MAX_TOKENS, PREFIX, RATE, ProgramTokens, TooMany

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
        # The list never has the token.
        listed = await p.list(A)
        assert listed == [{"id": made["id"], "label": made["label"], "created_at": 5000, "last_used_at": None}]
        assert await p.list(B) == []
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
        for _ in range(500):  # a program playing fast: half a second of requests
            assert await p.check(made["token"])
            clock[0] += 0.001
        assert writes == ["program-usage"]
        assert (await p.list(A))[0]["last_used_at"] == 1000
        clock[0] += LAST_USED_EVERY
        await p.check(made["token"])
        assert writes == ["program-usage"] * 2

    asyncio.run(go())


def test_revoke_is_immediate_and_only_for_the_owner():
    async def go():
        p = ProgramTokens(MemoryStore())
        mine, theirs = await p.create(A, "a"), await p.create(B, "b")
        assert await p.check(mine["token"])  # (writes a usage record)
        assert not await p.revoke(B, mine["id"])  # not B's to revoke
        assert await p.check(mine["token"])
        assert await p.revoke(A, mine["id"])
        assert await p.check(mine["token"]) is None and await p.list(A) == []
        assert not await p.revoke(A, mine["id"])
        assert await p.check(theirs["token"])
        assert await p.store.get("program-usage", mine["id"]) is None

    asyncio.run(go())


def test_at_most_five_tokens_an_account():
    async def go():
        p = ProgramTokens(MemoryStore())
        made = [await p.create(A, f"bot {n}") for n in range(MAX_TOKENS)]
        with pytest.raises(TooMany):
            await p.create(A, "one more")
        await p.create(B, "another account's")  # each account its own five
        await p.revoke(A, made[0]["id"])
        await p.create(A, "now there is room")
        assert len(await p.list(A)) == MAX_TOKENS
        assert (await p.create(B, ""))["label"] == "my bot"  # an empty label gets one

    asyncio.run(go())


def test_the_rate_limit_lets_a_burst_through_then_the_rate():
    async def go():
        clock = [0.0]
        p = ProgramTokens(MemoryStore(), clock=lambda: clock[0])
        prog = await p.check((await p.create(A, "fast"))["token"])
        other = await p.check((await p.create(A, "slow"))["token"])
        assert all(p.allow(prog) == 0 for _ in range(int(BURST)))
        wait = p.allow(prog)
        assert 0 < wait <= 1 / RATE
        assert p.allow(other) == 0  # each token its own bucket
        clock[0] += 1  # a second later: RATE more
        assert sum(p.allow(prog) == 0 for _ in range(int(RATE) + 5)) == int(RATE)

    asyncio.run(go())
