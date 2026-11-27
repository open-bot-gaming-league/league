"""Thin sync HTTP client for the Bot Poker table API.

Talks to the table's HTTP API (see the table's API.md). No game
logic lives here -- just request/response plumbing and error mapping.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from typing import Any


DEFAULT_BASE_URL = "http://localhost:9777/poker/"
DEFAULT_TIMEOUT = 10.0


class TableError(Exception):
    """A table API call answered with a non-2xx status.

    status: the HTTP status (400 bad input, 404 unknown session, 409 not
    possible right now -- stale hand/step or hand-over mismatch).
    message: the server's own `error` text, when it sent JSON; otherwise
    a short description.
    """

    def __init__(self, status: int, message: str):
        super().__init__(f"table API error {status}: {message}")
        self.status = status
        self.message = message


class TableClient:
    """Talks to one table server. Stateless: every call is a full request."""

    def __init__(self, base_url: str | None = None, timeout: float = DEFAULT_TIMEOUT):
        # Default: $OBGL_TABLE_URL, else DEFAULT_BASE_URL; the server and the
        # tests read the same variable.
        base_url = base_url or os.environ.get("OBGL_TABLE_URL") or DEFAULT_BASE_URL
        self.base_url = base_url if base_url.endswith("/") else base_url + "/"
        self.timeout = timeout

    def _get(self, path: str, **params: Any) -> dict:
        url = self.base_url + path
        qs = "&".join(f"{k}={urllib.request.quote(str(v))}" for k, v in params.items() if v is not None)
        if qs:
            url += "?" + qs
        return self._request(urllib.request.Request(url, method="GET"))

    def _post(self, path: str, body: dict) -> dict:
        data = json.dumps({k: v for k, v in body.items() if v is not None}).encode()
        req = urllib.request.Request(
            self.base_url + path,
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        return self._request(req)

    def _request(self, req: urllib.request.Request) -> dict:
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return json.load(resp)
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            try:
                message = json.loads(raw).get("error", raw)
            except json.JSONDecodeError:
                message = raw or e.reason
            raise TableError(e.code, message) from e
        except urllib.error.URLError as e:
            raise TableError(0, f"table unreachable: {e.reason}") from e

    def asset(self, path: str) -> tuple[int, str, bytes]:
        """A file of the table's page ("" is index.html): (status, content
        type, body). For the MCP server's /table, which serves the page the
        table binary embeds rather than a copy of it."""
        req = urllib.request.Request(self.base_url + path, method="GET")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.status, resp.headers.get("Content-Type", "application/octet-stream"), resp.read()
        except urllib.error.HTTPError as e:
            return e.code, "text/plain; charset=utf-8", b""
        except urllib.error.URLError as e:
            raise TableError(0, f"table unreachable: {e.reason}") from e

    # -- API.md endpoints --------------------------------------------------

    def info(self) -> dict:
        return self._get("api/info")

    def new(self, seat: str = "alternate", opponent: str | None = None) -> dict:
        return self._post("api/new", {"seat": seat, "opponent": opponent})

    def state(self, session: str, since: int | None = None) -> dict:
        """since: only the finished hands after that one, with their events
        (at most the table's last 200), for a page replaying them."""
        return self._get("api/state", session=session, since=since)

    def act(
        self,
        session: str,
        hand: int,
        step: int,
        action: int | None = None,
        kind: str | None = None,
        amount: int | None = None,
    ) -> dict:
        """Either form of api/act: `action`, a button's index (the old
        form, kept for one release), or `kind` with `amount` (any legal
        size; amount is the "raise to" chips -- see API.md "Bet sizes")."""
        return self._post(
            "api/act",
            {"session": session, "hand": hand, "step": step, "action": action, "kind": kind, "amount": amount},
        )

    def next(self, session: str, hand: int, seat: str | None = None, opponent: str | None = None) -> dict:
        return self._post(
            "api/next",
            {"session": session, "hand": hand, "seat": seat, "opponent": opponent},
        )

    def results(self, table_id: str) -> dict:
        """A ranked table's results: {id, placed, summary, text}. 404 when
        the table keeps no results under that id."""
        return self._get("api/results/" + urllib.request.quote(table_id, safe=""))
