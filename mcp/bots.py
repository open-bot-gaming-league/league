"""Submitted bots: a coach's program uploads a wasm module and, optionally,
a directory of data files; the server checks them and keeps them as a
numbered version of a named bot of the account's. Auth is the account's
program token (programs.py), on the table's API (bot_web.py has the routes).

    bot-submissions  submission id -> {account, name, wasm: {size, sha256},
                     data: [{path, size, sha256}], state, created_at,
                     sessions: {sha256: upload session} while open,
                     lease_until, attempts while a runner checks it,
                     version, bot, table once finalized, ...}
                     state: open (uploading) -> checking (queued to be checked)
                            -> finalized (a version) | rejected (problems)
                            | superseded (the account started another)
    account-bots     account id -> {versions: [{name, version, submission,
                     created_at, wasm, data_files, data_bytes}], open: submission id | None,
                     checking: submission id | None, asked: [when finalize was asked, the
                     last hour's]}
    finalize-queue   "queue" -> {submissions: [id, ...], started_at}: the
                     submissions waiting to be checked, in order

A submission declares every file's size and sha256 up front, so the caps
and the throttle are checked before a byte is uploaded. Then the client
uploads each distinct file the account does not already have (bot_files.py:
files are kept by sha256, per account, so a new version with the same data
uploads only its wasm) to the upload session the server started for it,
and asks to finalize. A session is kept with the submission, so a client
run again resumes a file where the last run stopped. Finalize queues the
submission to be checked, and the client asks after it until it has an
answer: the check (Bots.check, run by finalize.py's runner apart from the
request) reads every new file back, rejects the submission if any size or
sha256 differs from what was declared or the module does not pass the wasm
check, and otherwise files the version. An account has one submission
being checked at a time. An account has at most one open submission: starting another
drops the first and its uploads, unless it declares the same files under
the same name, which picks the first up where it left off.

Throttle: Limits.per_hour finalize requests per account in any hour
(OBGL_BOT_SUBMITS_PER_HOUR, default 1). Checked when a submission is made
and again when finalize is asked, before it is queued; a refused finalize
leaves it open. A request counts when it is made: a submission the check
rejects does not give its hour back. The account's record is written only by conditional
updates (AuthStore.update), so the request that would go over the limit is
refused even when two servers are asked at once.
"""

from __future__ import annotations

import hashlib
import logging
import re
import secrets
import subprocess
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass

import anyio
import anyio.to_thread

from bot_files import BotFiles, bot_key, upload_key
from jobs import JobStarter, kick, taken
from oauth.store import AuthStore

log = logging.getLogger("obgl.bots")

DAY = 24 * 60 * 60
HOUR = 60 * 60
FINALIZE_QUEUE = ("finalize-queue", "queue")
OPEN_FOR = 7 * DAY  # an open submission lasts as long as a Cloud Storage upload session
NAME = re.compile(r"[a-z0-9][a-z0-9-]{0,31}")
SHA256 = re.compile(r"[0-9a-f]{64}")
WASM_MAGIC = b"\0asm"
BUSY_CHECKING = "your last submission is still being checked: wait for its answer, then submit again"


@dataclass(frozen=True)
class Limits:
    wasm_bytes: int = 64 << 20
    data_bytes: int = 16 << 30
    data_files: int = 1000
    path_bytes: int = 200
    per_hour: int = 1  # finalized submissions per account in any hour (0: no limit)


class Refused(Exception):
    """A request the rules turn down: `status` for the HTTP answer, and the
    problems, each one sentence a coach (or their agent) can act on."""

    def __init__(self, status: int, problems: list[str], **extra):
        super().__init__("; ".join(problems))
        self.status = status
        self.problems = problems
        self.extra = extra


WasmCheck = Callable[[bytes], "str | None"]


class GoBotCheck:
    """The wasm check as the arena does it: `botcheck` (poker/arena/cmd/botcheck)
    reads the module on stdin and exits 0 if it can run as a bot, 1 with the
    reason on stderr if not. Any other ending is the checker's own failure,
    raised rather than blamed on the module."""

    def __init__(self, command: str, timeout: float = 120) -> None:
        self.command, self.timeout = command, timeout

    def __call__(self, wasm: bytes) -> str | None:
        done = subprocess.run([self.command, "-"], input=wasm, capture_output=True, timeout=self.timeout)
        if done.returncode == 0:
            return None
        reason = done.stderr.decode(errors="replace").strip()
        if done.returncode == 1 and reason:
            return reason
        raise RuntimeError(f"botcheck failed (exit {done.returncode}): {reason[-500:]}")


def _settled(sub: dict) -> dict:
    """A submission's record without what only an open or queued one has."""
    return {k: v for k, v in sub.items() if k not in ("expires_at", "sessions", "lease_until", "attempts")}


def _closed(index: dict | None, sid: str) -> dict:
    """The account's record with submission sid no longer its open one."""
    index = index or {"versions": [], "open": None}
    return {**index, "open": None if index.get("open") == sid else index.get("open")}


class FinalizeQueue:
    """The submissions waiting to be checked, in the order asked
    (finalize-queue "queue": {submissions: [id, ...], started_at}), and
    the start of their runner: the finalize job (starter: jobs.py), when
    one is added or a coach asks after one (jobs.kick). No starter: a
    runner in this process (finalize.py's Finalizer.serve) is woken
    instead."""

    def __init__(self, store: AuthStore, clock: Callable[[], float] = time.time,
                 starter: JobStarter | None = None) -> None:
        self.store, self.clock, self.starter = store, clock, starter
        self._woken: list[anyio.Event] = []

    async def submissions(self) -> list[str]:
        return (await self.store.get(*FINALIZE_QUEUE) or {"submissions": []})["submissions"]

    async def add(self, sid: str) -> None:
        def change(q):
            q = q or {"submissions": []}
            return None if sid in q["submissions"] else {**q, "submissions": [*q["submissions"], sid]}
        await self.store.update(*FINALIZE_QUEUE, change)
        for event in self._woken:
            event.set()
        await self.kick()

    async def remove(self, sid: str) -> None:
        def change(q):
            if q is None or sid not in q["submissions"]:
                return None
            return {**q, "submissions": [s for s in q["submissions"] if s != sid]}
        await self.store.update(*FINALIZE_QUEUE, change)

    async def taken(self) -> None:
        await taken(self.store, FINALIZE_QUEUE)

    async def kick(self) -> None:
        """Start the runner if the queue needs one (jobs.kick)."""
        if self.starter is not None:
            await kick(self.store, FINALIZE_QUEUE, "submissions", "bot-submissions", self.starter, self.clock, log,
                       "bots: a submission is to be checked")

    def wake_event(self) -> anyio.Event:
        """An event set when a submission is added (in this process)."""
        event = anyio.Event()
        self._woken.append(event)
        return event

    def forget_event(self, event: anyio.Event) -> None:
        self._woken.remove(event)


class Bots:
    def __init__(self, store: AuthStore, files: BotFiles, check: WasmCheck,
                 limits: Limits = Limits(), clock: Callable[[], float] = time.time,
                 on_finalized: Callable[[str, dict], Awaitable[dict]] | None = None,
                 queue: FinalizeQueue | None = None) -> None:
        """on_finalized(account, version) is called with each version filed
        (ranked.py queues its table) and returns the table ({id, ...}).
        queue: where submissions wait to be checked (default: one in the
        store that starts no job: a runner in this process checks them)."""
        self.store, self.files, self.wasm_check, self.limits, self.clock = store, files, check, limits, clock
        self.on_finalized = on_finalized
        self.queue = queue if queue is not None else FinalizeQueue(store, clock)
        self._locks: dict[str, anyio.Lock] = {}

    def _lock(self, account: str) -> anyio.Lock:
        lock = self._locks.get(account)
        if lock is None:
            lock = self._locks[account] = anyio.Lock()
        return lock

    async def _index(self, account: str) -> dict:
        return await self.store.get("account-bots", account) or {"versions": [], "open": None}

    def _throttle(self, index: dict) -> None:
        """Refused(429) if the account has asked to finalize its limit in
        the last hour (a submission the check refused counts)."""
        n = self.limits.per_hour
        if n <= 0:
            return
        now = self.clock()
        recent = sorted(t for t in index.get("asked", []) if t > now - HOUR)
        if len(recent) >= n:
            at = recent[-n] + HOUR
            bots = "one bot" if n == 1 else f"{n} bots"
            raise Refused(429, [f"You can submit {bots} an hour, refused ones included: try again at "
                                f"{time.strftime('%H:%M UTC', time.gmtime(at))}"], retry_at=int(at))

    def _manifest(self, body: Mapping) -> tuple[str, dict, list[dict]]:
        """(name, wasm, data) from a submission's JSON, or Refused with every problem in it."""
        lim, problems = self.limits, []

        def entry(e, what: str) -> dict | None:
            if not isinstance(e, Mapping) or not isinstance(e.get("size"), int) or isinstance(e.get("size"), bool) \
                    or e["size"] < 0 or not isinstance(e.get("sha256"), str) or not SHA256.fullmatch(e["sha256"]):
                problems.append(f"{what}: give its size (bytes) and sha256 (64 lowercase hex digits)")
                return None
            return {"size": e["size"], "sha256": e["sha256"]}

        name = body.get("name")
        if not isinstance(name, str) or not NAME.fullmatch(name):
            problems.append("name: 1-32 characters, lowercase letters, digits and '-', starting with a letter or digit")
        wasm = entry(body.get("wasm"), "wasm")
        if wasm is not None and wasm["size"] > lim.wasm_bytes:
            problems.append(f"wasm: {wasm['size']:,} bytes is over the {lim.wasm_bytes:,}-byte cap")
        if wasm is not None and wasm["size"] == 0:
            problems.append("wasm: the file is empty")
        raw = body.get("data", [])
        if not isinstance(raw, list):
            problems.append("data: a list of files")
            raw = []
        if len(raw) > lim.data_files:
            problems.append(f"data: {len(raw):,} files is over the {lim.data_files:,}-file cap")
        data, seen = [], set()
        for i, e in enumerate(raw[: lim.data_files]):
            path = e.get("path") if isinstance(e, Mapping) else None
            label = f"data file {path!r}" if isinstance(path, str) else f"data file {i + 1}"
            f = entry(e, label)
            if not isinstance(path, str) or not _clean_path(path) or len(path.encode()) > lim.path_bytes:
                problems.append(f"{label}: a relative path with '/' between parts (no '.', '..', empty or hidden parts), "
                                f"at most {lim.path_bytes} bytes")
            elif path in seen:
                problems.append(f"{label}: listed twice")
            seen.add(path)
            if f is not None and isinstance(path, str):
                data.append({"path": path, **f})
        for path in sorted(seen):
            parents = ["/".join(path.split("/")[:i]) for i in range(1, path.count("/") + 1)]
            if any(p in seen for p in parents):
                problems.append(f"data file {path!r}: its folder is also a file of the bot's")
        total = sum(f["size"] for f in data)
        if total > lim.data_bytes:
            problems.append(f"data: {total:,} bytes in all is over the {lim.data_bytes:,}-byte cap")
        if problems:
            raise Refused(400, problems)
        return name, wasm, sorted(data, key=lambda f: f["path"])

    # ---- the API ------------------------------------------------------------------

    async def create(self, account: str, body: Mapping) -> dict:
        """A new open submission (or the account's open one, or the one
        being checked, if it declares the same files under the same name):
        its status()."""
        name, wasm, data = self._manifest(body)
        async with self._lock(account):
            index = await self._index(account)
            for kept, state in ((index.get("checking"), "checking"), (index["open"], "open")):
                old = await self.store.get("bot-submissions", kept) if kept else None
                if old is not None and old["state"] == state and (state == "checking" or old["expires_at"] > self.clock()) \
                        and (old["name"], old["wasm"], old["data"]) == (name, wasm, data):
                    return await self._status(kept, old)
            self._throttle(index)
            if index["open"]:
                await self._drop(index["open"], await self.store.get("bot-submissions", index["open"]), "superseded")
            sid = secrets.token_hex(16)
            now = int(self.clock())
            sub = {"account": account, "name": name, "wasm": wasm, "data": data, "state": "open",
                   "created_at": now, "expires_at": now + OPEN_FOR}
            await self.store.put("bot-submissions", sid, sub)
            await self.store.update("account-bots", account, lambda i: {**(i or {"versions": []}), "open": sid})
            return await self._status(sid, sub)

    async def status(self, account: str, sid: str) -> dict:
        """The submission now. One being checked starts the check's runner
        if none is on it (the start when it was queued may have failed)."""
        async with self._lock(account):
            sub = await self._own(account, sid)
            if sub["state"] == "checking":
                await self.queue.kick()
            return await self._status(sid, sub)

    async def finalize(self, account: str, sid: str) -> dict:
        """Queue the submission to be checked (finalize.py checks it: every
        file and the module, then files the version or rejects it): its
        status(), state checking. Asked again while it is checked: the same.
        Refused(409) if it is not open, or another of the account's is
        being checked; Refused(429) if the account is over its hourly limit
        (it stays open)."""
        async with self._lock(account):
            sub = await self._own(account, sid)
            if sub["state"] == "checking":
                return await self._status(sid, sub)
            if sub["state"] != "open":
                raise Refused(409, [f"this submission is {sub['state']}" +
                                    (f": {'; '.join(sub['problems'])}" if sub.get("problems") else "")])
            index = await self._index(account)
            self._throttle(index)
            other = index.get("checking")
            if other and other != sid:
                o = await self.store.get("bot-submissions", other)
                if o is not None and o["state"] == "checking":
                    raise Refused(409, [BUSY_CHECKING])

            def ask(i):
                i = i or {"versions": [], "open": None}
                if i.get("checking") != other:
                    raise Refused(409, [BUSY_CHECKING])  # another server queued one since the look above
                self._throttle(i)
                now = self.clock()
                asked = [t for t in i.get("asked", []) if t > now - HOUR]  # the last hour's: what the limit counts
                return {**_closed(i, sid), "checking": sid, "asked": [*asked, int(now)]}

            await self.store.update("account-bots", account, ask)
            sub = {k: v for k, v in sub.items() if k not in ("expires_at", "sessions")}
            sub.update(state="checking", requested_at=int(self.clock()))
            await self.store.put("bot-submissions", sid, sub)
            await self.queue.add(sid)
            return await self._status(sid, sub)

    # ---- the check (finalize.py's runner calls these) ------------------------------------

    def pending(self, sub: dict | None) -> bool:
        """Whether a queued submission has work left: its check, or (once
        its version is filed) its ranked table."""
        if sub is None:
            return False
        return sub["state"] == "checking" or (sub["state"] == "finalized" and "table" not in sub
                                              and self.on_finalized is not None)

    async def check(self, sid: str) -> None:
        """Check a queued submission (every file read back, sizes and sha256
        against what it declared, the module by the wasm check), then file
        its version and queue its ranked table, or reject it. Each step is
        written as it is done, so a check stopped part way is done again
        from where it stopped."""
        sub = await self.store.get("bot-submissions", sid)
        if sub is None:
            return
        account = sub["account"]
        if sub["state"] == "checking":
            problems = await anyio.to_thread.run_sync(lambda: self._verify(account, sid, sub))
            if problems:
                await self.reject(sid, sub, problems)
                return
            filed = {}

            def file(i):
                i = i or {"versions": [], "open": None}
                done = next((v for v in i["versions"] if v.get("submission") == sid), None)  # filed by a stopped check
                if done is None:
                    version = 1 + max((v["version"] for v in i["versions"] if v["name"] == sub["name"]), default=0)
                    done = {"name": sub["name"], "version": version, "submission": sid, "created_at": int(self.clock()),
                            "wasm": sub["wasm"], "data_files": len(sub["data"]),
                            "data_bytes": sum(f["size"] for f in sub["data"])}
                filed["entry"] = done
                return {**i, "versions": [*(v for v in i["versions"] if v.get("submission") != sid), done],
                        "checking": None if i.get("checking") == sid else i.get("checking")}

            await self.store.update("account-bots", account, file)
            entry = filed["entry"]
            sub = _settled(sub)
            sub.update(state="finalized", version=entry["version"], finalized_at=entry["created_at"], bot=entry)
            await self.store.put("bot-submissions", sid, sub)
        if self.pending(sub):  # finalized, its table not yet queued
            table = await self.on_finalized(account, sub["bot"])
            await self.store.update("bot-submissions", sid, lambda s: None if s is None else {**_settled(s), "table": table["id"]})

    async def reject(self, sid: str, sub: dict, problems: list[str]) -> None:
        """End a queued submission without a version: its uploads removed."""
        await self._drop(sid, sub, "rejected", problems)
        await self.store.update("account-bots", sub["account"],
                                lambda i: None if i is None or i.get("checking") != sid else {**i, "checking": None})

    async def list(self, account: str) -> dict:
        index = await self._index(account)
        return {"bots": sorted(index["versions"], key=lambda v: (v["name"], v["version"])), "open": index["open"]}

    # ---- the steps ------------------------------------------------------------------

    async def _own(self, account: str, sid: str) -> dict:
        sub = await self.store.get("bot-submissions", sid) if re.fullmatch(r"[0-9a-f]{32}", sid) else None
        if sub is None or sub["account"] != account:
            raise Refused(404, ["no such submission"])
        if sub["state"] == "open" and sub["expires_at"] <= self.clock():
            raise Refused(409, ["this submission has expired: start again"])
        return sub

    def _blobs(self, sub: dict) -> dict[str, int]:
        """sha256 -> size of every distinct file the submission names."""
        return {f["sha256"]: f["size"] for f in [sub["wasm"], *sub["data"]]}

    async def _status(self, sid: str, sub: dict) -> dict:
        """The submission as the API shows it; for each file still to
        upload, its session (started now if it has none: call with the
        account's lock held)."""
        sessions = sub.get("sessions", {})

        def look() -> tuple[list[dict], bool]:
            out, started = [], False
            for sha, size in self._blobs(sub).items():
                if self.files.size(bot_key(sub["account"], sha)) == size:
                    continue  # the account has it, checked, from before
                key = upload_key(sub["account"], sid, sha)
                done = self.files.size(key) == size
                u = {"sha256": sha, "size": size, "done": done}
                if not done and sub["state"] == "open":
                    if sha not in sessions:
                        sessions[sha], started = self.files.start_upload(key, size), True
                    u["session"] = sessions[sha]
                out.append(u)
            return out, started

        uploads, started = await anyio.to_thread.run_sync(look)
        if started:
            sub["sessions"] = sessions
            await self.store.put("bot-submissions", sid, sub)
        out = {"id": sid, "name": sub["name"], "state": sub["state"], "created_at": sub["created_at"],
               "wasm": sub["wasm"], "data": sub["data"], "uploads": uploads}
        for k in ("version", "problems", "expires_at", "requested_at", "bot", "table"):
            if k in sub:
                out[k] = sub[k]
        return out

    def _verify(self, account: str, sid: str, sub: dict) -> list[str]:
        """Read back every uploaded file (sizes first, then the module, then
        the data); promote them if all is well. The problems, or []."""
        names = {sub["wasm"]["sha256"]: "wasm"}
        for f in sub["data"]:
            names.setdefault(f["sha256"], f"data file {f['path']!r}")
        todo, problems = [], []
        for sha, size in self._blobs(sub).items():
            if self.files.size(bot_key(account, sha)) == size:
                continue
            key = upload_key(account, sid, sha)
            got = self.files.size(key)
            if got is None:
                problems.append(f"{names[sha]}: not uploaded")
            elif got != size:
                problems.append(f"{names[sha]}: {got:,} bytes uploaded, {size:,} declared")
            else:
                todo.append((sha, key))
        if problems:
            return problems
        wasm_sha = sub["wasm"]["sha256"]
        module = b"".join(self.files.read(self._source(account, wasm_sha, todo)))
        if hashlib.sha256(module).hexdigest() != wasm_sha:
            return ["wasm: its sha256 is not the one declared (the file changed, or the upload was damaged)"]
        if not module.startswith(WASM_MAGIC):
            return ["wasm: not a WebAssembly module (it does not start with \\0asm)"]
        reason = self.wasm_check(module)
        if reason:
            return [f"wasm: {reason}"]
        for sha, key in todo:
            if sha == wasm_sha:
                continue
            h = hashlib.sha256()
            for piece in self.files.read(key):
                h.update(piece)
            if h.hexdigest() != sha:
                problems.append(f"{names[sha]}: its sha256 is not the one declared (the file changed, or the upload was damaged)")
        if problems:
            return problems
        for sha, key in todo:
            self.files.promote(key, bot_key(account, sha))
        return []

    def _source(self, account: str, sha: str, todo: list) -> str:
        """Where the file with this sha256 is now: its upload, or the account's copy."""
        return next((key for s, key in todo if s == sha), bot_key(account, sha))

    async def _drop(self, sid: str, sub: dict | None, state: str, problems: list[str] | None = None) -> None:
        """End an open submission: its uploads removed, its record kept as `state`."""
        if sub is None:
            return
        def remove() -> None:
            for sha in self._blobs(sub):
                self.files.delete(upload_key(sub["account"], sid, sha))
        await anyio.to_thread.run_sync(remove)
        sub = _settled(sub)
        sub["state"] = state
        if problems:
            sub["problems"] = problems
        await self.store.put("bot-submissions", sid, sub)


def _clean_path(path: str) -> bool:
    parts = path.split("/")
    return all(p and p not in (".", "..") and not p.startswith(".") and "\\" not in p and "\0" not in p for p in parts)
