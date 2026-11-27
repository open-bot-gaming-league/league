"""Starting the ranked-table job (jobs.py) when a table is queued
(RankedTables.enqueue): once per table queued while no runner holds the
queue's head, never while one does, and a start that fails leaves the table
queued for the next. A fake starter and a fake HTTP session: no GCP."""

import logging
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import MemoryStore
from ranked import QUEUE, RankedTables
from jobs import START_GRACE, CloudRunJob, starter_from_env

ME = "github:1001"
JOB = "projects/p/locations/us-west1/jobs/obgl-ranked"


class Clock:
    def __init__(self):
        self.t = 1_800_000_000.0

    def __call__(self):
        return self.t


class Starter:
    def __init__(self, fail=False):
        self.starts, self.fail = 0, fail

    def start(self):
        if self.fail:
            raise RuntimeError("jobs.run: HTTP 403: permission denied (a test)")
        self.starts += 1
        return f"execution {self.starts}"


def _tables(starter):
    clock = Clock()
    return RankedTables(MemoryStore(), clock, starter=starter), clock


async def _enqueue(tables, n=1):
    return await tables.enqueue(ME, {"name": "mybot", "version": n, "submission": f"sub-{n}"})


async def _lease(tables, tid, until):
    """The table as a runner has it: running, its lease until `until`."""
    t = await tables.store.get("ranked-tables", tid)
    await tables.store.put("ranked-tables", tid, {**t, "state": "running", "attempts": 1, "lease_until": until})


@pytest.mark.asyncio
async def test_a_table_queued_with_no_runner_starts_one():
    starter = Starter()
    tables, _ = _tables(starter)
    table = await _enqueue(tables)
    assert starter.starts == 1
    assert table["state"] == "queued" and table["position"] == 1


@pytest.mark.asyncio
async def test_no_start_while_a_runner_holds_the_head():
    starter = Starter()
    tables, clock = _tables(starter)
    first = await _enqueue(tables)
    await _lease(tables, first["id"], clock() + 120)
    second = await _enqueue(tables, 2)
    assert starter.starts == 1  # the runner plays on to the second
    assert second["position"] == 2
    # Its lease run out (the runner died): the next table queued starts one.
    clock.t += 121
    await _enqueue(tables, 3)
    assert starter.starts == 2


@pytest.mark.asyncio
async def test_a_failed_start_keeps_the_table_queued_and_the_next_start_takes_both(caplog):
    starter = Starter(fail=True)
    tables, clock = _tables(starter)
    with caplog.at_level(logging.ERROR, logger="obgl.ranked"):
        first = await _enqueue(tables)
    assert first["state"] == "queued" and first["position"] == 1
    assert (await tables.store.get("ranked-tables", first["id"]))["state"] == "queued"
    assert any(first["id"] in r.getMessage() and "did not start" in r.getMessage() for r in caplog.records)
    assert "HTTP 403" in caplog.text  # the error itself is in the log
    starter.fail = False
    second = await _enqueue(tables, 2)
    assert starter.starts == 0  # tried a moment ago: the next try waits START_GRACE
    clock.t += START_GRACE + 1
    await tables.list(ME)  # the coach looks at their tables: the start is made up
    assert starter.starts == 1
    assert (await tables.store.get("ranked-queue", QUEUE))["tables"] == [first["id"], second["id"]]
    await tables.list(ME)
    assert starter.starts == 1  # started: not again until the grace is over


@pytest.mark.asyncio
async def test_without_a_starter_nothing_is_started():
    tables = RankedTables(MemoryStore())
    table = await _enqueue(tables)
    assert table["position"] == 1


def test_the_job_comes_from_the_environment():
    assert starter_from_env({}, "OBGL_RANKED_JOB") is None
    assert starter_from_env({"OBGL_RANKED_JOB": ""}, "OBGL_RANKED_JOB") is None
    job = starter_from_env({"OBGL_RANKED_JOB": JOB}, "OBGL_RANKED_JOB")
    assert isinstance(job, CloudRunJob) and job.name == JOB
    for bad in ("obgl-ranked", "projects/p/locations/us-west1/jobs/", "projects/p/jobs/obgl-ranked",
                f"https://run.googleapis.com/v2/{JOB}"):
        with pytest.raises(ValueError, match="OBGL_RANKED_JOB: .*projects/<project>/locations/<region>/jobs/<name>"):
            starter_from_env({"OBGL_RANKED_JOB": bad}, "OBGL_RANKED_JOB")


class Response:
    def __init__(self, status, body):
        self.status_code, self._body = status, body
        self.text = str(body)

    def json(self):
        return self._body


class Session:
    """An authorized session as CloudRunJob uses one: post(), recorded."""

    def __init__(self, response):
        self.response, self.posts = response, []

    def post(self, url, json=None, timeout=None):
        self.posts.append((url, json, timeout))
        return self.response


def test_cloud_run_job_calls_jobs_run():
    op = f"{JOB.rsplit('/jobs/', 1)[0]}/operations/abc"
    session = Session(Response(200, {"name": op, "metadata": {}}))
    assert CloudRunJob(JOB, session).start() == op
    assert session.posts == [(f"https://run.googleapis.com/v2/{JOB}:run", {}, 10)]


def test_cloud_run_job_raises_when_the_api_refuses():
    session = Session(Response(403, {"error": {"code": 403, "message": "Permission 'run.jobs.run' denied"}}))
    with pytest.raises(RuntimeError, match=r"HTTP 403: .*run\.jobs\.run"):
        CloudRunJob(JOB, session).start()
