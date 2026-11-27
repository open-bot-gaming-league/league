"""Starting the ranked-table job: the server, when it queues a table and no
runner holds the queue's head, starts an execution of the Cloud Run job
that plays the queue (deploy/cloudrun/RANKED-TABLES.md). ranked.py decides
when; a JobStarter does the starting.

    OBGL_RANKED_JOB   projects/<project>/locations/<region>/jobs/<name>: the
                      job to start; unset: nothing is started (a table waits
                      until a runner is started some other way)

The server's account needs roles/run.invoker on the job (jobs.run).
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from typing import Any, Protocol

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


def starter_from_env(env: Mapping[str, str]) -> JobStarter | None:
    name = env.get("OBGL_RANKED_JOB")
    return CloudRunJob(name) if name else None
