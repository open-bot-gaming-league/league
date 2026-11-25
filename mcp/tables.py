"""Who owns a table session and who is playing it: the record behind the
browser table's Play and Watch (table_web.py) and the MCP tools'
control checks (server.py).

A *table session* is the Go table's session (cmd/table/API.md): one
stranger's run of hands against a bot. The Go table knows nothing about
accounts; this module is where a session gets an owner and a controller.

    owner       the signed-in account that opened it ("github:<id>",
                "google:<sub>"), through the connector
                (new_session) or the browser table (api/new). Fixed.
    controller  who may act in it right now, exactly one of:
                  browser          the owner, in the browser table (any tab)
                  agent <conn>     the owner's agent on one MCP connection
                  agent (waiting)  handed to the owner's agent, not claimed yet
                Everyone else -- the owner's other agents, a browser
                while an agent plays -- may only read.
    watch       the session's watch token: what a watch link carries
                (/table/watch/<token>). It names the session for reading
                only, and only the owner can read through it. It is never
                the table session id, so a leaked watch link cannot act.
    switches    every change of controller: when (`at`), at which hand and
                step, from and to (browser | agent | unclaimed), and `by`
                (browser | agent: who asked). Shown in the browser's hand history and
                returned by the my_sessions() tool.

Every read is owner-scoped (table_session_for, by_watch, list_for): another
account's session looks exactly like one that does not exist, so a page
can only answer 404 for both. The controller does not change the pending
decision: the table state is the session's, so whoever takes over next
sees the same hand and step and makes that decision.

Records live in the OAuth store (oauth/store.py), so they survive a
restart with a durable store, as the tokens that name their owners do:

    table-sessions   table session id -> {owner, created_at, watch,
                     controller: {kind, agent}, switches: [...]}
    watch-tokens     sha256(watch token) -> {session}
    account-tables   account id -> {sessions: [table session ids, newest first]}

Not stored: `lock(session)` serialises a control check with the action
it guards (one process: Cloud Run runs at most one instance, see
deploy/cloudrun/CLOUDRUN.md), and `observe()` keeps each session's last
finished-hand state, so a watcher who polled past the end of a hand can
still be shown how it ended (table_web.py, api/watch).
"""

from __future__ import annotations

import hashlib
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass

import anyio

from oauth.store import AuthStore, Record

BROWSER = "browser"
AGENT = "agent"
LIST_LIMIT = 20  # sessions remembered per account (newest first); older ones drop off the list


def _hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


@dataclass(frozen=True)
class Party:
    """Who is asking to act: the browser (the owner, signed in at the table)
    or the agent on one MCP connection (`conn`: server.py's per-connection
    key)."""

    kind: str
    conn: str | None = None

    @classmethod
    def browser(cls) -> Party:
        return cls(BROWSER)

    @classmethod
    def agent(cls, conn: str) -> Party:
        return cls(AGENT, conn)

    def as_controller(self) -> dict:
        return {"kind": self.kind, "agent": self.conn}


class NotInControl(Exception):
    """The session is the caller's owner's, but someone else is playing it.
    `record` is the session record, for saying who."""

    def __init__(self, message: str, record: Record):
        super().__init__(message)
        self.record = record


def controller_name(controller: dict) -> str:
    """The controller in words, from the owner's point of view."""
    if controller["kind"] == BROWSER:
        return "you, in the browser"
    if controller.get("agent") is None:
        return "your agent (waiting for it to claim the session)"
    return "your agent"


def _label(controller: dict) -> str:
    """A controller in a switch record: browser, agent, or unclaimed (handed
    to the owner's agent, none has claimed it yet)."""
    if controller["kind"] == AGENT and controller.get("agent") is None:
        return "unclaimed"
    return controller["kind"]


def is_controller(record: Record, party: Party) -> bool:
    c = record["controller"]
    return c["kind"] == party.kind and (party.kind == BROWSER or c.get("agent") == party.conn)


class TableSessions:
    def __init__(self, store: AuthStore, clock: Callable[[], float] = time.time) -> None:
        self.store = store
        self.clock = clock
        self._locks: dict[str, anyio.Lock] = {}
        self._finals: dict[str, dict] = {}  # session -> {hand number: that hand's final state}

    # ---- records ----------------------------------------------------------

    async def create(self, session_id: str, owner: str, controller: Party) -> Record:
        """File a new table session: owner, first controller, a fresh watch
        token, and its place at the top of the owner's list."""
        watch = secrets.token_urlsafe(24)
        record = {
            "owner": owner,
            "created_at": int(self.clock()),
            "watch": watch,
            "controller": controller.as_controller(),
            "switches": [],
        }
        await self.store.put("table-sessions", session_id, record)
        await self.store.put("watch-tokens", _hash(watch), {"session": session_id})
        index = await self.store.get("account-tables", owner) or {"sessions": []}
        index["sessions"] = [session_id, *[s for s in index["sessions"] if s != session_id]][:LIST_LIMIT]
        await self.store.put("account-tables", owner, index)
        return record

    async def table_session_for(self, session_id: str, account: str) -> Record | None:
        """The record, only if `account` owns the session. There is
        deliberately no owner-blind read: another account's session looks
        the same as a session that does not exist."""
        record = await self.store.get("table-sessions", session_id)
        if record is None or record["owner"] != account:
            return None
        return record

    async def by_watch(self, token: str, account: str) -> tuple[str, Record] | None:
        """(session id, record) for a watch token, only for the owner."""
        entry = await self.store.get("watch-tokens", _hash(token)) if token else None
        if entry is None:
            return None
        record = await self.table_session_for(entry["session"], account)
        return (entry["session"], record) if record is not None else None

    async def list_for(self, account: str) -> list[tuple[str, Record]]:
        """The account's sessions, newest first."""
        index = await self.store.get("account-tables", account) or {"sessions": []}
        out = []
        for session_id in index["sessions"]:
            record = await self.table_session_for(session_id, account)
            if record is not None:
                out.append((session_id, record))
        return out

    # ---- control ------------------------------------------------------------

    def lock(self, session_id: str) -> anyio.Lock:
        """Hold this across a control check and the action it allows (or a
        switch), so a switch cannot land between the two."""
        lock = self._locks.get(session_id)
        if lock is None:
            lock = self._locks[session_id] = anyio.Lock()
        return lock

    async def require_control(self, session_id: str, account: str, party: Party) -> Record:
        """The record if `party` may act now. LookupError: no such session
        for this account. NotInControl: the owner's, but someone else is
        playing it."""
        record = await self.table_session_for(session_id, account)
        if record is None:
            raise LookupError("no such session")
        if not is_controller(record, party):
            raise NotInControl(f"{controller_name(record['controller'])} is playing this session", record)
        return record

    async def switch(
        self, session_id: str, account: str, to: Party, *, hand: int, step: int, by: Party | None = None
    ) -> Record:
        """Make `to` the controller (hold lock(session_id)), and log it.
        `by`: who asked, when not `to` itself (the browser tagging its agent
        in). A switch to whoever already controls is not logged. The
        decision pending at `hand`/`step` is the new controller's."""
        record = await self.table_session_for(session_id, account)
        if record is None:
            raise LookupError("no such session")
        new = to.as_controller()
        if record["controller"] != new:
            record["switches"].append(
                {
                    "at": int(self.clock()),
                    "hand": hand,
                    "step": step,
                    "from": _label(record["controller"]),
                    "to": _label(new),
                    "by": (by or to).kind,
                }
            )
            record["controller"] = new
            await self.store.put("table-sessions", session_id, record)
        return record

    # ---- finished hands, for watchers --------------------------------------

    def observe(self, session_id: str, state: dict) -> None:
        """Every table state that passes through this server (an act, a
        next hand, a page load): a finished hand's final state is kept
        (the latest two hands), so a watcher who polled past it can still
        be shown how it ended."""
        if state.get("over"):
            finals = self._finals.setdefault(session_id, {})
            finals[state["hand"]] = state
            for hand in sorted(finals)[:-2]:
                del finals[hand]

    def final_state(self, session_id: str, hand: int) -> dict | None:
        return self._finals.get(session_id, {}).get(hand)
