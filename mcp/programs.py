"""Program tokens: a bearer token a user makes on the table page and gives
to their own program (the Go SDK's sdk.Table reads it from OBGL_TOKEN), so
the program can play that user's table over the table's API. An account
has one at a time: it can revoke it, or regenerate it (revoke and make a
new one, shown once).

A program token is not an OAuth token. It lives in its own record kinds,
so the one place that accepts it is the table's API (table_web.py), and
there only on its owner's table; /mcp, which reads `access` records only,
never sees it as valid. A program cannot make, read or revoke a token,
reset the table or change its settings: those are the signed-in
browser's.

    program-tokens    sha256(token) -> {account, id, label, created_at}
    account-programs  account id -> {tokens: [{id, key, label, created_at}]}  (key: the sha256; at most one)
    program-usage     token id -> {account, last_used_at}

The token itself (`obgl_pt_` + 256 random bits) is shown once, in the
answer that makes it, and stored only as its sha256, as access tokens
are. No expiry: a token works until it is revoked; `last_used_at` is
shown on the page so a forgotten one can be spotted. It is written at
most once a minute per token, so a program playing at full speed does
not cost a store write per move, and in a record of its own, so that
write can never bring a revoked token back.

Rate: each token may make RATE requests a second on average, BURST at
once (an in-process token bucket: one Cloud Run instance, see
deploy/cloudrun/CLOUDRUN.md), counted by the token's hash before any store
read, so a made-up token is limited as a real one is. What a token is
(its program, or nothing) is remembered for CACHE_TTL seconds; revoke()
forgets it at once. Volume belongs in the SDK's local runner.
"""

from __future__ import annotations

import hashlib
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass

from oauth.store import AuthStore

PREFIX = "obgl_pt_"
MAX_TOKENS = 1  # one program token an account: revoke it or regenerate it
LABEL_MAX = 40
RATE, BURST = 20.0, 40.0
LAST_USED_EVERY = 60  # seconds between writes of a token's last_used_at
CACHE_TTL = 60  # seconds a token's lookup (a program, or none) is remembered
MAX_TRACKED = 10_000  # tokens remembered (buckets, lookups) before the stale ones are dropped


def _hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


@dataclass(frozen=True)
class Program:
    """A program token, as a request presents it."""

    account: str
    id: str
    label: str


class TooMany(Exception):
    """The account already has its program token."""


class RateLimited(Exception):
    """A token over its rate; `wait`: seconds until its next request may go."""

    def __init__(self, wait: float):
        super().__init__(f"over the rate: wait {wait:.2f} s")
        self.wait = wait


class ProgramTokens:
    def __init__(self, store: AuthStore, clock: Callable[[], float] = time.time) -> None:
        self.store = store
        self.clock = clock
        self._used: dict[str, float] = {}  # key -> when last_used_at was last written
        self._buckets: dict[str, tuple[float, float]] = {}  # key -> (requests left, at)
        self._cache: dict[str, tuple[Program | None, float]] = {}  # key -> (what the token is, until when)

    async def create(self, account: str, label: str) -> dict:
        """The account's program token: {token, id, label, created_at}. The
        only time the token is ever returned. TooMany if it has one already
        (an account has one at a time: regenerate() replaces it)."""
        index = await self.store.get("account-programs", account) or {"tokens": []}
        if len(index["tokens"]) >= MAX_TOKENS:
            raise TooMany("you have a program token already: revoke it, or regenerate it for a new one")
        label = " ".join(label.split())[:LABEL_MAX] or "my bot"
        token = PREFIX + secrets.token_urlsafe(32)
        entry = {"id": secrets.token_hex(4), "key": _hash(token), "label": label, "created_at": int(self.clock())}
        await self.store.put(
            "program-tokens", entry["key"],
            {"account": account, "id": entry["id"], "label": label, "created_at": entry["created_at"]},
        )
        index["tokens"].append(entry)
        await self.store.put("account-programs", account, index)
        return {"token": token, "id": entry["id"], "label": label, "created_at": entry["created_at"]}

    async def get(self, account: str) -> dict | None:
        """The account's program token, without the token: {id, label,
        created_at, last_used_at}; None when it has none."""
        index = await self.store.get("account-programs", account) or {"tokens": []}
        if not index["tokens"]:
            return None
        entry = index["tokens"][0]
        usage = await self.store.get("program-usage", entry["id"]) or {}
        return {"id": entry["id"], "label": entry["label"], "created_at": entry["created_at"],
                "last_used_at": usage.get("last_used_at")}

    async def revoke(self, account: str) -> bool:
        """Revoke the account's program token; False if it has none.
        Immediate: the next request with it is refused."""
        index = await self.store.get("account-programs", account) or {"tokens": []}
        if not index["tokens"]:
            return False
        for e in index["tokens"]:
            await self.store.delete("program-tokens", e["key"])
            await self.store.delete("program-usage", e["id"])
            self._used.pop(e["key"], None)
            self._cache.pop(e["key"], None)
        await self.store.put("account-programs", account, {"tokens": []})
        return True

    async def regenerate(self, account: str, label: str | None = None) -> dict:
        """Revoke the account's token (if any) and make a new one, as
        create() returns it; the label is the old one's unless given. The
        old token is refused from here on."""
        old = await self.get(account)
        await self.revoke(account)
        return await self.create(account, label if label is not None else (old["label"] if old else ""))

    async def check(self, token: str) -> Program | None:
        """The program a bearer token belongs to, or None (not a program
        token, or revoked). RateLimited, before anything is read, when this
        token (known or not) is over its rate. What a token is, either
        way, is remembered for CACHE_TTL seconds, so a program playing fast,
        or someone trying made-up tokens, costs at most one store read a
        token a minute; revoke() forgets its token at once."""
        if not token.startswith(PREFIX):
            return None
        key = _hash(token)
        wait = self._take(key)
        if wait:
            raise RateLimited(wait)
        now = self.clock()
        hit = self._cache.get(key)
        if hit is not None and hit[1] > now:
            program = hit[0]
        else:
            record = await self.store.get("program-tokens", key)
            program = Program(record["account"], record["id"], record["label"]) if record else None
            self._remember(key, program, now)
        if program is not None and now - self._used.get(key, 0) >= LAST_USED_EVERY:
            self._used[key] = now
            await self.store.put("program-usage", program.id, {"account": program.account, "last_used_at": int(now)})
        return program

    def _remember(self, key: str, program: Program | None, now: float) -> None:
        if len(self._cache) >= MAX_TRACKED:
            self._cache = {k: v for k, v in self._cache.items() if v[1] > now}
        self._cache[key] = (program, now + CACHE_TTL)

    def _take(self, key: str) -> float:
        """Take one request from the token's bucket: 0 if it may go ahead,
        else the seconds until it may."""
        now = self.clock()
        if len(self._buckets) >= MAX_TRACKED:  # full buckets are the same as none
            self._buckets = {k: v for k, v in self._buckets.items() if v[0] + (now - v[1]) * RATE < BURST}
        left, at = self._buckets.get(key, (BURST, now))
        left = min(BURST, left + (now - at) * RATE)
        if left < 1:
            self._buckets[key] = (left, now)
            return (1 - left) / RATE
        self._buckets[key] = (left - 1, now)
        return 0.0
