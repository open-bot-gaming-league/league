"""The Cloud Run jobs the server starts, and what their runners share. A
job's runner drains a queue in the auth store: it takes the queue's head
under a lease (a time, `lease_until`, on the head's record) that it renews
while it works, and exits when the queue is empty. The server, when it
queues something and no runner holds the head, starts an execution.

    OBGL_RANKED_JOB     the ranked-table job (ranked.py), started when a table is queued
    OBGL_FINALIZE_JOB   the job that checks submitted bots (finalize.py), started when
                        a submission is to be checked (unset: the server checks them
                        itself, for local runs)
                        each: projects/<project>/locations/<region>/jobs/<name>;
                        unset: no job is started

Starting one needs roles/run.invoker on the job (jobs.run, no overrides).
Each is started when work is queued and again when someone asks after
work that waits with no runner (kick): a start that failed, or a runner
that died, is made up by the next ask.
"""

from __future__ import annotations

import logging
import re
from collections.abc import Callable, Mapping
from contextlib import asynccontextmanager
from typing import Any, Protocol

import anyio
import anyio.to_thread

RUN_API = "https://run.googleapis.com/v2"
SCOPE = "https://www.googleapis.com/auth/cloud-platform"
JOB_NAME = re.compile(r"projects/[^/\s]+/locations/[^/\s]+/jobs/[^/\s]+")


class JobStarter(Protocol):
    def start(self) -> str:
        """Start one execution of the job: what was started, for the log.
        Raises if it could not be started."""
        ...


class CloudRunJob:
    """A Cloud Run job, started with the Run Admin API's jobs.run, as this
    process's own account (Application Default Credentials). `session` is
    an authorized requests session; by default one made on first use."""

    def __init__(self, name: str, session: Any = None, timeout: float = 10) -> None:
        if not JOB_NAME.fullmatch(name):
            raise ValueError(f"a Cloud Run job's name is projects/<project>/locations/<region>/jobs/<name>, got {name!r}")
        self.name, self.timeout, self._session = name, timeout, session

    def _authorized(self):
        if self._session is None:
            import google.auth
            from google.auth.transport.requests import AuthorizedSession

            credentials, _ = google.auth.default(scopes=[SCOPE])
            self._session = AuthorizedSession(credentials)
        return self._session

    def start(self) -> str:
        r = self._authorized().post(f"{RUN_API}/{self.name}:run", json={}, timeout=self.timeout)
        if not 200 <= r.status_code < 300:
            raise RuntimeError(f"jobs.run {self.name}: HTTP {r.status_code}: {r.text[:500]}")
        return r.json().get("name", self.name)  # the execution's long-running operation


def starter_from_env(env: Mapping[str, str], var: str) -> JobStarter | None:
    """The job `var` (OBGL_RANKED_JOB, OBGL_FINALIZE_JOB) names, or None."""
    name = env.get(var)
    try:
        return CloudRunJob(name) if name else None
    except ValueError as e:
        raise ValueError(f"{var}: {e}") from None


START_GRACE = 120  # after a start, time for its runner to take the queue's head


def held(record: Mapping | None, now: float) -> bool:
    """Whether a runner holds this queue head under a live lease."""
    return record is not None and record.get("lease_until", 0) > now


async def start(starter: JobStarter, log: logging.Logger, what: str) -> bool:
    """Start an execution; a failure is logged, not raised. Whether it started."""
    try:
        started = await anyio.to_thread.run_sync(starter.start)
    except Exception:
        log.exception("%s, but its runner did not start", what)
        return False
    log.info("%s; started its runner: %s", what, started)
    return True


async def kick(store, queue: tuple[str, str], field: str, kind: str, starter: JobStarter,
               clock: Callable[[], float], log: logging.Logger, what: str) -> None:
    """Start a runner for a queue (the record `queue`, its ids in `field`,
    each a `kind` record) if it has work and no runner: unless the head's
    lease is live, or a start was made in the last START_GRACE seconds (its
    `started_at`, which a runner clears when it takes the head: taken()).
    Called when work is queued and when someone asks after it, so a start
    that failed, or a runner that died, is made up by the next ask. A
    failure is logged, not raised."""
    try:
        q = await store.get(*queue)
        if not q or not q[field]:
            return
        now, head = clock(), q[field][0]
        seen = q.get("started_at", 0)
        if held(await store.get(kind, head), now) or now - seen < START_GRACE:
            return
        mine = await store.update(*queue, lambda r: None if r is None or r.get("started_at", 0) != seen
                                  else {**r, "started_at": now})
    except Exception:
        log.exception("%s: its queue could not be read to start its runner", what)
        return
    if mine is not None:  # else another server started it just now
        await start(starter, log, f"{what} (the queue's head: {head})")


async def taken(store, queue: tuple[str, str]) -> None:
    """A runner holds the queue's head now: its lease, not a recent start,
    is what keeps another from being started (kick)."""
    await store.update(*queue, lambda r: None if not r or "started_at" not in r
                       else {k: v for k, v in r.items() if k != "started_at"})


def with_runner(app, runner):
    """app (a Starlette app: the server's) with runner.serve() running for
    as long as the app serves."""
    inner = app.router.lifespan_context

    @asynccontextmanager
    async def lifespan(a):
        async with inner(a) as state:
            async with anyio.create_task_group() as tg:
                tg.start_soon(runner.serve)
                try:
                    yield state
                finally:
                    tg.cancel_scope.cancel()

    app.router.lifespan_context = lifespan
    return app
