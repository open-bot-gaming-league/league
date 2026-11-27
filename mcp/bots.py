"""Submitted bots: a coach's program uploads a wasm module and, optionally,
a directory of data files; the server checks them and keeps them as a
numbered version of a named bot of the account's. Auth is the account's
program token (programs.py), on the table's API (bot_web.py has the routes).

    bot-submissions  submission id -> {account, name, wasm: {size, sha256},
                     data: [{path, size, sha256}], state, created_at,
                     sessions: {sha256: upload session} while open, ...}
                     state: open (uploading) -> finalized (a version) | rejected (problems)
                            | superseded (the account started another)
    account-bots     account id -> {versions: [{name, version, submission,
                     created_at, wasm, data_files, data_bytes}], open: submission id | None}

A submission declares every file's size and sha256 up front, so the caps
and the throttle are checked before a byte is uploaded. Then the client
uploads each distinct file the account does not already have (bot_files.py:
files are kept by sha256, per account, so a new version with the same data
uploads only its wasm) to the upload session the server started for it,
and asks to finalize. A session is kept with the submission, so a client
run again resumes a file where the last run stopped. Finalize reads every new file
back, refuses the submission if any size or sha256 differs from what was
declared or the module does not pass the wasm check, and otherwise files
the version. An account has at most one open submission: starting another
drops the first and its uploads, unless it declares the same files under
the same name, which picks the first up where it left off.

Throttle: Limits.per_day finalized submissions per account in any 24 hours
(OBGL_BOT_SUBMITS_PER_DAY, default 1). Checked when a submission is made
and again when it is finalized; a refused finalize leaves it open.
"""

from __future__ import annotations

import hashlib
import re
import secrets
import subprocess
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass

import anyio
import anyio.to_thread

from bot_files import BotFiles, bot_key, upload_key
from oauth.store import AuthStore

DAY = 24 * 60 * 60
OPEN_FOR = 7 * DAY  # an open submission lasts as long as a Cloud Storage upload session
NAME = re.compile(r"[a-z0-9][a-z0-9-]{0,31}")
SHA256 = re.compile(r"[0-9a-f]{64}")
WASM_MAGIC = b"\0asm"


@dataclass(frozen=True)
class Limits:
    wasm_bytes: int = 64 << 20
    data_bytes: int = 16 << 30
    data_files: int = 1000
    path_bytes: int = 200
    per_day: int = 1


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


def _now_iso(t: float) -> str:
    return time.strftime("%Y-%m-%d %H:%M UTC", time.gmtime(t))


class Bots:
    def __init__(self, store: AuthStore, files: BotFiles, check: WasmCheck,
                 limits: Limits = Limits(), clock: Callable[[], float] = time.time) -> None:
        self.store, self.files, self.check, self.limits, self.clock = store, files, check, limits, clock
        self._locks: dict[str, anyio.Lock] = {}

    def _lock(self, account: str) -> anyio.Lock:
        lock = self._locks.get(account)
        if lock is None:
            lock = self._locks[account] = anyio.Lock()
        return lock

    async def _index(self, account: str) -> dict:
        return await self.store.get("account-bots", account) or {"versions": [], "open": None}

    def _throttle(self, index: dict) -> None:
        if self.limits.per_day <= 0:
            return
        now = self.clock()
        recent = sorted(v["created_at"] for v in index["versions"] if v["created_at"] > now - DAY)
        if len(recent) >= self.limits.per_day:
            at = recent[-self.limits.per_day] + DAY
            raise Refused(429, [f"one account may submit {self.limits.per_day} bot version(s) a day: "
                                f"the next can be finalized from {_now_iso(at)}"], retry_at=int(at))

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
        total = sum(f["size"] for f in data)
        if total > lim.data_bytes:
            problems.append(f"data: {total:,} bytes in all is over the {lim.data_bytes:,}-byte cap")
        if problems:
            raise Refused(400, problems)
        return name, wasm, sorted(data, key=lambda f: f["path"])

    # ---- the API ------------------------------------------------------------------

    async def create(self, account: str, body: Mapping) -> dict:
        """A new open submission (or the account's open one, if it declares
        the same files under the same name): its status()."""
        name, wasm, data = self._manifest(body)
        async with self._lock(account):
            index = await self._index(account)
            self._throttle(index)
            if index["open"]:
                old = await self.store.get("bot-submissions", index["open"])
                if old is not None and old["state"] == "open" and old["expires_at"] > self.clock() \
                        and (old["name"], old["wasm"], old["data"]) == (name, wasm, data):
                    return await self._status(index["open"], old)
                await self._drop(index["open"], old, "superseded")
            sid = secrets.token_hex(16)
            now = int(self.clock())
            sub = {"account": account, "name": name, "wasm": wasm, "data": data, "state": "open",
                   "created_at": now, "expires_at": now + OPEN_FOR}
            await self.store.put("bot-submissions", sid, sub)
            index["open"] = sid
            await self.store.put("account-bots", account, index)
            return await self._status(sid, sub)

    async def status(self, account: str, sid: str) -> dict:
        async with self._lock(account):
            return await self._status(sid, await self._own(account, sid))

    async def finalize(self, account: str, sid: str) -> dict:
        """Check every file and the module; file the version: {bot: version}.
        Refused(422) with the problems (and the submission rejected) if any
        file or the module is wrong; Refused(409) if it is not open,
        Refused(429) if the account is over its throttle (it stays open)."""
        async with self._lock(account):
            sub = await self._own(account, sid)
            if sub["state"] != "open":
                raise Refused(409, [f"this submission is {sub['state']}" +
                                    (f": {'; '.join(sub['problems'])}" if sub.get("problems") else "")])
            index = await self._index(account)
            self._throttle(index)
            problems = await anyio.to_thread.run_sync(lambda: self._verify(account, sid, sub))
            if problems:
                await self._drop(sid, sub, "rejected", problems)
                index["open"] = None
                await self.store.put("account-bots", account, index)
                raise Refused(422, problems)
            version = 1 + max((v["version"] for v in index["versions"] if v["name"] == sub["name"]), default=0)
            now = int(self.clock())
            entry = {"name": sub["name"], "version": version, "submission": sid, "created_at": now, "wasm": sub["wasm"],
                     "data_files": len(sub["data"]), "data_bytes": sum(f["size"] for f in sub["data"])}
            sub = {k: v for k, v in sub.items() if k not in ("expires_at", "sessions")}
            sub.update(state="finalized", version=version, finalized_at=now)
            await self.store.put("bot-submissions", sid, sub)
            index["versions"].append(entry)
            index["open"] = None
            await self.store.put("account-bots", account, index)
            return {"bot": entry}

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
        for k in ("version", "problems", "expires_at"):
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
        reason = self.check(module)
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
        sub = {k: v for k, v in sub.items() if k not in ("expires_at", "sessions")}
        sub["state"] = state
        if problems:
            sub["problems"] = problems
        await self.store.put("bot-submissions", sid, sub)


def _clean_path(path: str) -> bool:
    parts = path.split("/")
    return all(p and p not in (".", "..") and not p.startswith(".") and "\\" not in p and "\0" not in p for p in parts)
