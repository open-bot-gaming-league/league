"""Ranked tables: every bot version a coach finalizes (bots.py) becomes a
ranked table -- the bot and ten seats drawn from the house bots, every pair
heads-up on duplicate deals (package league; poker/arena runs every seat as
wasm in the sandbox) -- and the coach finds it in their list and reads its
results (the table's results/<id> page; the table_results tool).

    ranked-tables   table id -> {account, bot, version, submission, seed, state,
                    queued_at, started_at, finished_at, attempts, lease_until,
                    reason, place, players}
                    state: queued -> running -> done | failed (with a reason)
    account-ranked  account id -> {tables: [table id, ...]}, oldest first: the
                    only way to an account's tables (none is ever looked up by guess)
    ranked-queue    "queue" -> {tables: [table id, ...]}: the tables not yet
                    finished, in the order they were queued
    account-badges  the badges each account's tables earned (badges.py): the
                    runner awards a table's before it marks the table done

A finished table's record (package league's JSON) is kept in a RecordStore
under the table's id, which is also its results' id: /table/results/<id>.
The record names the bot's owner (league.Owner): the account, the bot, the
version and the submission.

Who writes what. Whoever checks submissions (finalize.py: the server, or
the finalize job), on filing a version, files the table (queued; its id
comes from the submission's, so a filing done again files no second
table), then appends it to the queue and to the account's list. From
then on only the runner writes the table, and one runner plays at a time:
the server's own (OBGL_RANKED_RUNNER=in-process, the local stack) or the
Cloud Run job's (`python ranked.py run`, deploy/cloudrun/RANKED-TABLES.md),
which is started when a table is queued and no runner holds the queue's
head (OBGL_RANKED_JOB, jobs.py). The runner takes the queue's head, marks
it running under a lease it renews while the table plays, plays it
(`rankedtable play`, a subprocess), files the record, marks the table
done or failed, and only then takes it off the queue. A runner
stopped anywhere in that loses nothing: the next one finds the head running
with its lease run out and plays it again (the same seed: the same table),
up to MAX_STARTS starts, after which the table fails as interrupted. A head
whose lease is live is another runner's: this one leaves it.

Tables play in the order queued, one at a time; nothing here limits how
many an account queues but bots.py's throttle on finalized versions.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import re
import secrets
import subprocess
import tempfile
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field, replace
from typing import Protocol

import anyio
import anyio.to_thread

from badges import Badges, BadgeSettings, settings_from_env as badge_settings_from_env
from bot_files import bot_key
from oauth.store import AuthStore
from jobs import JobStarter, kick, taken

log = logging.getLogger("obgl.ranked")

QUEUE = "queue"  # the ranked-queue record's key
MAX_STARTS = 3
HOUSE_POOL = ("dove", "clam", "shark", "turtle", "ox")  # league.HousePool
TABLE_ID = re.compile(r"[0-9a-f]{32}")


class RecordStore(Protocol):
    def put(self, table_id: str, record: bytes) -> None:
        """Keep a finished table's record under its id, replacing any."""
        ...


class RecordDir:
    """Records as <id>.json in one directory: what the Go table serves
    with -records (league.RecordDir). Written whole or not at all."""

    def __init__(self, path: str) -> None:
        self.path = os.path.abspath(path)
        os.makedirs(self.path, exist_ok=True)

    def put(self, table_id: str, record: bytes) -> None:
        if not TABLE_ID.fullmatch(table_id):
            raise ValueError(f"table id {table_id!r}")
        fd, tmp = tempfile.mkstemp(dir=self.path, prefix=".record-", suffix=".tmp")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(record)
                f.flush()
                os.fsync(f.fileno())
            os.chmod(tmp, 0o644)
            os.replace(tmp, os.path.join(self.path, table_id + ".json"))
        except BaseException:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise


class RankedTables:
    """The queue, each account's list of tables and the badges they earned
    (badges.py), in the auth store."""

    def __init__(self, store: AuthStore, clock: Callable[[], float] = time.time,
                 badges: BadgeSettings = BadgeSettings(), starter: JobStarter | None = None) -> None:
        """starter: started when a table is queued and no runner holds the
        queue's head (jobs.py: the Cloud Run job); None: none is."""
        self.store, self.clock, self.starter = store, clock, starter
        self.badges = Badges(store, badges, clock)
        self._woken: list[anyio.Event] = []

    async def enqueue(self, account: str, version: Mapping) -> dict:
        """The ranked table for a version bots.py just filed: its view. A
        version has one table (its id comes from the submission's), so a
        filing done again (a runner stopped part way) queues nothing new."""
        tid = table_id(version["submission"])
        new = {"account": account, "bot": version["name"], "version": version["version"],
               "submission": version["submission"], "seed": secrets.randbits(63),
               "state": "queued", "queued_at": int(self.clock()), "attempts": 0}
        kept = {}

        def file(t):
            kept["table"] = t if t is not None else new
            return None if t is not None else new

        await self.store.update("ranked-tables", tid, file)
        table = kept["table"]
        if table["state"] == "queued":
            await self.store.update("ranked-queue", QUEUE, lambda q: _with(q, tid))
        await self.store.update("account-ranked", account, lambda a: _with(a, tid))
        for event in self._woken:
            event.set()
        await self.kick()
        return await self._view(tid, table)

    async def view(self, tid: str) -> dict | None:
        """A table as its account's list shows it, or None."""
        table = await self.store.get("ranked-tables", tid)
        return None if table is None else await self._view(tid, table)

    async def kick(self) -> None:
        """Start the job if the queue needs a runner (jobs.kick): when a
        table is queued, and when a coach asks after their tables."""
        if self.starter is not None:
            await kick(self.store, ("ranked-queue", QUEUE), "tables", "ranked-tables", self.starter, self.clock, log,
                       "ranked: a table is queued")

    async def list(self, account: str) -> dict:
        """The account's tables, newest first; a done one with the names
        of the badges it earned."""
        index = await self.store.get("account-ranked", account) or {"tables": []}
        queue = await self.queue()
        if any(tid in queue for tid in index["tables"]):
            await self.kick()  # one of theirs waits: make up a start that failed, or a runner that died
        earned: dict[str, list[str]] = {}
        for b in await self.badges.all(account):
            earned.setdefault(b["table"], []).append(self.badges.name(b))
        out = []
        for tid in reversed(index["tables"]):
            table = await self.store.get("ranked-tables", tid)
            if table is not None and table["account"] == account:
                out.append(self._show(tid, table, queue))
                if table["state"] == "done":
                    out[-1]["badges"] = earned.get(tid, [])
        return {"tables": out}

    async def newest(self, account: str) -> dict | None:
        tables = (await self.list(account))["tables"]
        return tables[0] if tables else None

    async def queue(self) -> list[str]:
        """The tables not yet finished, the next to play first."""
        return (await self.store.get("ranked-queue", QUEUE) or {"tables": []})["tables"]

    async def _view(self, tid: str, table: dict) -> dict:
        return self._show(tid, table, await self.queue())

    @staticmethod
    def _show(tid: str, table: dict, queue: list[str]) -> dict:
        out = {"id": tid, "bot": table["bot"], "version": table["version"], "state": table["state"],
               "queued_at": table["queued_at"]}
        for k in ("started_at", "finished_at", "reason", "place", "players"):
            if k in table:
                out[k] = table[k]
        if table["state"] == "queued" and tid in queue:
            out["position"] = queue.index(tid) + 1  # 1: plays next
        if table["state"] == "done":
            out["result"] = tid
        return out

    def wake_event(self) -> anyio.Event:
        """An event set when a table is queued (in this process)."""
        event = anyio.Event()
        self._woken.append(event)
        return event

    def forget_event(self, event: anyio.Event) -> None:
        self._woken.remove(event)


@dataclass(frozen=True)
class TableSettings:
    """How ranked tables are played: rankedtable play's flags."""

    pool: tuple[str, ...] = HOUSE_POOL
    seats: int = 10  # league.DefaultSeats: eleven players
    hands: int = 1_000_000  # a pair
    parallel: int = 0  # pairs at once (0: the runner's GOMAXPROCS)
    draw: str = "copies"  # league.Draws: copies of a pool bot allowed


@dataclass
class Runner:
    """Plays queued tables, one at a time, with rankedtable (command), the
    house bots' guests (guests: prebuilt <id>.wasm files or Go packages),
    the bot store's files as a directory (files_dir: its keys are paths
    under it), and the records filed in records."""

    tables: RankedTables
    command: str
    guests: str
    files_dir: str
    records: RecordStore
    settings: TableSettings = field(default_factory=TableSettings)
    timeout: float = 4 * 3600  # a table's wall time before it is failed
    lease: float = 120  # how long a running table is this runner's without a renewal
    env: Mapping[str, str] | None = None  # rankedtable's environment (None: this process's)

    @property
    def store(self) -> AuthStore:
        return self.tables.store

    async def step(self) -> bool:
        """Play the queue's head, or settle it: False when there is
        nothing this runner can do (the queue is empty, or its head is
        another runner's)."""
        queue = await self.tables.queue()
        if not queue:
            return False
        tid = queue[0]
        table = await self.store.get("ranked-tables", tid)
        now = self.tables.clock()
        if table is None or table["state"] in ("done", "failed"):
            await self._dequeue(tid)  # its account was deleted, or a runner stopped after finishing it
            return True
        if table["state"] == "running" and table["lease_until"] > now:
            return False
        if table["attempts"] >= MAX_STARTS:
            await self._finish(tid, table, "failed", reason=f"the table was interrupted {table['attempts']} times without finishing")
            return True
        seen = (table["state"], table.get("lease_until"), table["attempts"])

        def claim(t):
            # Only the table as read: another runner that claimed it first wins.
            if t is None or (t["state"], t.get("lease_until"), t["attempts"]) != seen:
                return None
            t = {k: v for k, v in t.items() if k != "reason"}
            return {**t, "state": "running", "started_at": int(now), "attempts": t["attempts"] + 1, "lease_until": now + self.lease}

        table = await self.store.update("ranked-tables", tid, claim)
        if table is None:
            return True  # it changed under us: look again
        await taken(self.store, ("ranked-queue", QUEUE))
        log.info("ranked: table %s (%s v%s of %s): start %d", tid, table["bot"], table["version"], table["account"], table["attempts"])
        try:
            record, reason = await self._play(tid, table)
        except Exception:
            # The runner's own failure (no rankedtable, the store down), not
            # the table's: it is queued again, and this start does not count.
            await self._requeue(tid)
            raise
        except BaseException:
            # Stopped (the server shutting down): the next start plays it, at
            # once. This start counts, as a crash's does: a table that stops
            # every runner that plays it is failed after MAX_STARTS.
            with anyio.CancelScope(shield=True):
                await self._renew(tid, 0)
            raise
        if record is not None:
            try:
                place, players = _placing(record)
            except (ValueError, KeyError, StopIteration) as e:
                log.error("ranked: table %s: its record does not read: %r", tid, e)
                record, reason = None, "the table's record could not be read"
        if record is None:
            await self._finish(tid, table, "failed", reason=reason)
            return True
        if await self.store.get("ranked-tables", tid) is None:
            await self._dequeue(tid)  # deleted while it played: keep nothing of it
            return True
        await anyio.to_thread.run_sync(lambda: self.records.put(tid, record))
        # Before the table is done: a runner stopped between the two plays
        # it again, and the award is once per table.
        badges = await self.tables.badges.award(tid, {**table, "place": place, "players": players})
        await self._finish(tid, table, "done", place=place, players=players)
        if badges:
            log.info("ranked: table %s earned %s", tid, ", ".join(b["badge"] for b in badges))
        return True

    async def drain(self) -> None:
        """Play until the queue is empty (or its head is another runner's)."""
        while await self.step():
            pass

    async def serve(self, poll: float = 30) -> None:
        """Play tables as they are queued, for as long as it runs; looks at
        the queue when a table is queued in this process, and every poll
        seconds."""
        while True:
            event = self.tables.wake_event()
            try:
                await self.drain()
                with anyio.move_on_after(poll):
                    await event.wait()
            except anyio.get_cancelled_exc_class():
                raise
            except Exception:
                log.exception("ranked: the runner failed; trying again in %s s", poll)
                await anyio.sleep(poll)
            finally:
                self.tables.forget_event(event)

    # ---- the steps -------------------------------------------------------------

    async def _requeue(self, tid: str) -> None:
        def change(t):
            if t is None or t["state"] != "running":
                return None
            t = {k: v for k, v in t.items() if k not in ("lease_until", "started_at")}
            return {**t, "state": "queued", "attempts": t["attempts"] - 1}
        await self.store.update("ranked-tables", tid, change)

    async def _renew(self, tid: str, until: float) -> None:
        def change(t):
            if t is None or t["state"] != "running":
                return None
            return {**t, "lease_until": until}
        await self.store.update("ranked-tables", tid, change)

    async def _finish(self, tid: str, table: dict, state: str, **fields) -> None:
        def change(t):
            if t is None:
                return None  # the account was deleted
            t = {k: v for k, v in t.items() if k != "lease_until"}
            t.update(state=state, finished_at=int(self.tables.clock()), **fields)
            return t
        await self.store.update("ranked-tables", tid, change)
        await self._dequeue(tid)
        log.info("ranked: table %s %s%s", tid, state, f": {fields['reason']}" if "reason" in fields else "")

    async def _dequeue(self, tid: str) -> None:
        def change(q):
            if q is None or tid not in q["tables"]:
                return None
            return {**q, "tables": [t for t in q["tables"] if t != tid]}
        await self.store.update("ranked-queue", QUEUE, change)

    async def _challenger(self, table: dict) -> tuple[dict | None, str | None]:
        """rankedtable's challenger spec for the table's bot version, or why there is none."""
        account = table["account"]
        sub = await self.store.get("bot-submissions", table["submission"])
        if sub is None or sub.get("account") != account or sub.get("state") != "finalized" \
                or (sub["name"], sub.get("version")) != (table["bot"], table["version"]):
            return None, "the bot version is no longer on file"
        data = sorted(sub["data"], key=lambda f: f["path"])
        files = {"wasm": sub["wasm"], **{f["path"]: f for f in data}}
        paths = {}
        for name, f in files.items():
            path = os.path.join(self.files_dir, bot_key(account, f["sha256"]))
            try:
                size = os.stat(path).st_size
            except OSError:
                size = None
            if size != f["size"]:
                log.error("ranked: %s: %s is %s bytes, %d on file", table["submission"], path, size, f["size"])
                return None, "the bot's files are not in the bot store"
            paths[name] = path
        version = f"wasm sha256:{sub['wasm']['sha256']}"
        if data:
            listing = "".join(f"{f['sha256']}  {f['path']}\n" for f in data)
            version += f" data sha256:{hashlib.sha256(listing.encode()).hexdigest()}"
        return {
            "name": f"{table['bot']} v{table['version']}",
            "version": version,
            "wasm": paths.pop("wasm"),
            "data": paths,
            "owner": {"account": account, "bot": table["bot"], "version": table["version"], "submission": table["submission"]},
        }, None

    async def _play(self, tid: str, table: dict) -> tuple[bytes | None, str | None]:
        """The table's record, or (None, why it has none)."""
        spec, reason = await self._challenger(table)
        if spec is None:
            return None, reason
        s = self.settings
        with tempfile.TemporaryDirectory(prefix="ranked-") as work:
            spec_path, out, errors = (os.path.join(work, n) for n in ("challenger.json", "table.json", "stderr.txt"))
            with open(spec_path, "w", encoding="utf-8") as f:
                json.dump(spec, f)
            args = [self.command, "play", "-challenger-spec", spec_path, "-wasm-pool", "-guests", os.path.abspath(self.guests),
                    "-pool", ",".join(s.pool), "-seats", str(s.seats), "-hands", str(s.hands), "-seed", str(table["seed"]),
                    "-parallel", str(s.parallel), "-draw", s.draw, "-out", out]
            with open(errors, "wb") as err:
                code = await self._run(tid, args, err)
            with open(errors, "rb") as f:
                stderr = f.read().decode(errors="replace")
            if code != 0:
                log.error("ranked: table %s: rankedtable exit %s:\n%s", tid, code, stderr[-4000:])
                if code is None:
                    return None, f"the table did not finish in {self.timeout / 3600:g} hours"
                last = stderr.strip().splitlines()[-1:] or [""]
                return None, f"the table did not finish (exit {code}): {last[0][:300]}"
            log.info("ranked: table %s:\n%s", tid, stderr)
            with open(out, "rb") as f:
                return f.read(), None

    async def _run(self, tid: str, args: list[str], err) -> int | None:
        """Run rankedtable, renewing the lease as it plays: its exit
        code, or None if it ran past the timeout (and was killed)."""
        proc = await anyio.open_process(args, stdin=subprocess.DEVNULL, stdout=err, stderr=err,
                                        env=dict(self.env) if self.env is not None else None)
        try:
            async with anyio.create_task_group() as tg:
                async def renew():
                    while True:
                        await anyio.sleep(self.lease / 3)
                        await self._renew(tid, self.tables.clock() + self.lease)
                tg.start_soon(renew)
                with anyio.move_on_after(self.timeout):
                    await proc.wait()
                tg.cancel_scope.cancel()
            return proc.returncode
        finally:
            if proc.returncode is None:
                proc.kill()
                with anyio.CancelScope(shield=True):
                    await proc.wait()


def table_id(submission: str) -> str:
    """The ranked table's id for a finalized submission's version."""
    return hashlib.sha256(f"ranked table {submission}".encode()).hexdigest()[:32]


def _with(record: dict | None, tid: str) -> dict | None:
    """A {"tables": [...]} record with tid at the end (None: it has it already)."""
    tables = (record or {"tables": []})["tables"]
    return None if tid in tables else {**(record or {}), "tables": [*tables, tid]}


def _placing(record: bytes) -> tuple[int, int]:
    """The challenger's place, and the players, from a league record."""
    r = json.loads(record)
    if r.get("format") != 1:
        raise ValueError(f"record format {r.get('format')}")
    place = next(p["place"] for p in r["places"] if p["seat"] == 0)
    return place, len(r["seats"])


def settings_from_env(env: Mapping[str, str]) -> TableSettings:
    s = TableSettings()
    if env.get("OBGL_RANKED_POOL"):
        s = replace(s, pool=tuple(p.strip() for p in env["OBGL_RANKED_POOL"].split(",") if p.strip()))
    for name, var in (("seats", "OBGL_RANKED_SEATS"), ("hands", "OBGL_RANKED_HANDS"), ("parallel", "OBGL_RANKED_PARALLEL")):
        if env.get(var):
            s = replace(s, **{name: int(env[var])})
    if env.get("OBGL_RANKED_DRAW"):
        s = replace(s, draw=env["OBGL_RANKED_DRAW"])
    return s


def runner_from_env(env: Mapping[str, str], tables: RankedTables) -> Runner:
    """The runner as the environment sets it up:

        OBGL_RANKEDTABLE      the rankedtable binary (poker/arena/cmd/rankedtable)
        OBGL_RANKED_GUESTS    the house bots' guests: <id>.wasm files, or poker/arena/guests
        OBGL_RECORDS          where records go: a directory (the Go table's -records)
        OBGL_BOT_FILES_DIR    the bot store's files as a directory; for OBGL_BOT_STORE=local:<dir>,
                              that dir; for a gcs: store, the bucket mounted (Cloud Storage FUSE)
        OBGL_RANKED_POOL, _SEATS, _HANDS, _PARALLEL, _DRAW   TableSettings (defaults: the ranked table)
        OBGL_RANKED_TIMEOUT   a table's wall time before it is failed, seconds (default 4 hours)
        OBGL_HOUSE_CACHE      where rankedtable finds the house bots' data (sdk.HouseCacheDir)
    """
    missing = [v for v in ("OBGL_RANKEDTABLE", "OBGL_RANKED_GUESTS", "OBGL_RECORDS") if not env.get(v)]
    if missing:
        raise ValueError(f"the ranked-table runner needs {', '.join(missing)}")
    files_dir = env.get("OBGL_BOT_FILES_DIR")
    if not files_dir:
        kind, _, where = env.get("OBGL_BOT_STORE", "").partition(":")
        if kind != "local" or not where:
            raise ValueError("the ranked-table runner needs OBGL_BOT_FILES_DIR (the bot store's files as a directory)")
        files_dir = where
    if not os.access(env["OBGL_RANKEDTABLE"], os.X_OK):
        raise ValueError(f"OBGL_RANKEDTABLE={env['OBGL_RANKEDTABLE']} is not a program this process can run")
    if not os.path.isdir(env["OBGL_RANKED_GUESTS"]):
        raise ValueError(f"OBGL_RANKED_GUESTS={env['OBGL_RANKED_GUESTS']} is not a directory")
    runner = Runner(tables, env["OBGL_RANKEDTABLE"], env["OBGL_RANKED_GUESTS"], files_dir, RecordDir(env["OBGL_RECORDS"]),
                    settings_from_env(env), env=env)
    if env.get("OBGL_RANKED_TIMEOUT"):
        runner.timeout = float(env["OBGL_RANKED_TIMEOUT"])
    return runner


def main() -> None:
    """The Cloud Run job's command: `python ranked.py run` plays every
    queued table, one at a time, and exits when the queue is empty."""
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("run", help="play the queued tables, then exit")
    ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    from oauth.store import store_from_env

    tables = RankedTables(store_from_env(os.environ), badges=badge_settings_from_env(os.environ))
    runner = runner_from_env(os.environ, tables)
    anyio.run(runner.drain)


if __name__ == "__main__":
    main()
