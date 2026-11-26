"""Program tokens: a bearer token a user makes on the table page and gives
to their own program (the Go SDK's sdk.Table reads it from OBGL_TOKEN), so
the program can play that user's table over the table's API.

A program token is not an OAuth token. It lives in its own record kinds,
so the one place that accepts it is the table's API (table_web.py), and
there only on its owner's table; /mcp, which reads `access` records only,
never sees it as valid. A program cannot make, list or revoke tokens,
reset the table or change its settings: those are the signed-in
browser's.

    program-tokens    sha256(token) -> {account, id, label, created_at}
    account-programs  account id -> {tokens: [{id, key, label, created_at}]}  (key: the sha256)
    program-usage     token id -> {account, last_used_at}

The token itself (`obgl_pt_` + 256 random bits) is shown once, in the
answer that makes it, and stored only as its sha256, as access tokens
are. No expiry: a token works until it is revoked; `last_used_at` is
shown in the list so a forgotten one can be spotted. It is written at
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
MAX_TOKENS = 5
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
    """The account already has MAX_TOKENS tokens."""


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
        """A new token for `account`: {token, id, label, created_at}. The
        only time the token is ever returned."""
        index = await self.store.get("account-programs", account) or {"tokens": []}
        if len(index["tokens"]) >= MAX_TOKENS:
            raise TooMany(f"you have {MAX_TOKENS} program tokens already: revoke one first")
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

    async def list(self, account: str) -> list[dict]:
        """The account's tokens, oldest first, without the tokens:
        {id, label, created_at, last_used_at}."""
        index = await self.store.get("account-programs", account) or {"tokens": []}
        out = []
        for entry in index["tokens"]:
            usage = await self.store.get("program-usage", entry["id"]) or {}
            out.append({"id": entry["id"], "label": entry["label"], "created_at": entry["created_at"],
                        "last_used_at": usage.get("last_used_at")})
        return out

    async def revoke(self, account: str, token_id: str) -> bool:
        """Revoke one of the account's tokens by id; False if it has none
        by that id. Immediate: the next request with it is refused."""
        index = await self.store.get("account-programs", account) or {"tokens": []}
        keep = [e for e in index["tokens"] if e["id"] != token_id]
        if len(keep) == len(index["tokens"]):
            return False
        for e in index["tokens"]:
            if e["id"] == token_id:
                await self.store.delete("program-tokens", e["key"])
                await self.store.delete("program-usage", e["id"])
                self._used.pop(e["key"], None)
                self._cache.pop(e["key"], None)
        index["tokens"] = keep
        await self.store.put("account-programs", account, index)
        return True

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
