"""Each account's one table, and who is at it: the record behind the
browser table (table_web.py), the MCP tools (server.py) and programs with
the account's program token (programs.py).

A *table session* is the Go table's session (cmd/table/API.md): a run of
hands against a bot with its running score. The Go table knows nothing
about accounts; this module gives each account exactly one table, its
newest table session:

    account-tables   account id -> {sessions: [table session ids, newest first]}
    table-sessions   table session id -> {owner, created_at}

The table is made the first time anything asks for it (ensure), and
replaced only by a reset (the browser's "Reset score": a new table
session, the score from zero). Older sessions stay on record but are no
longer anyone's table. An account from before one-table-per-login gets
its newest session as its table, as it stands.

Who may play it: the owner, in any of three ways -- the browser, the
owner's agent (on any MCP connection), a program with the owner's program
token -- and whoever acts first on the pending decision makes it. There
is no controller to hand over: the Go table refuses a move meant for a
decision that has already been made (409, a stale hand or step), and
`lock(session)` serialises the moves made through this server with the
bookkeeping around them.

Who is at the table (`presence`): the last move (who, when, at which
hand and step), when the agent and each program were last seen, and who
made the moves of each recent hand. Kept in memory only (Cloud Run runs
one instance: deploy/cloudrun/CLOUDRUN.md), so a program moving many
times a second costs no store writes; a restart forgets it, and the page
just stops saying who moved last.

Not stored either: `observe()` keeps each session's last finished-hand
state, so a page that polled past the end of a hand can still be shown
how it ended (table_web.py, api/state).
"""

from __future__ import annotations

import time
from collections.abc import Callable
from dataclasses import dataclass

import anyio
import anyio.to_thread

from oauth.store import AuthStore
from table_client import TableClient, TableError

BROWSER = "browser"
AGENT = "agent"
PROGRAM = "program"
LIST_LIMIT = 20  # sessions remembered per account (newest first); older ones drop off the list
HANDS_KEPT = 50  # recent hands whose players presence remembers


@dataclass(frozen=True)
class Party:
    """Who is asking: the owner in the browser, the owner's agent on one
    MCP connection (`key`: server.py's per-connection key), or a program
    (`key`: its token id, `label`: what the owner called it)."""

    kind: str
    key: str | None = None
    label: str | None = None

    @classmethod
    def browser(cls) -> Party:
        return cls(BROWSER)

    @classmethod
    def agent(cls, conn: str) -> Party:
        return cls(AGENT, conn)

    @classmethod
    def program(cls, token_id: str, label: str) -> Party:
        return cls(PROGRAM, token_id, label)

    @property
    def name(self) -> str:
        """In words, for the owner: "you", "your agent", "your program
        “my bot”"."""
        if self.kind == BROWSER:
            return "you"
        if self.kind == AGENT:
            return "your agent"
        return f"your program \u201c{self.label}\u201d"


class Tables:
    def __init__(self, store: AuthStore, client: TableClient, clock: Callable[[], float] = time.time) -> None:
        self.store = store
        self.client = client
        self.clock = clock
        self._locks: dict[str, anyio.Lock] = {}
        self._finals: dict[str, dict] = {}  # session -> {hand number: that hand's final state}
        self._last: dict[str, dict] = {}  # session -> the last move: {kind, name, at, hand, step}
        self._seen: dict[str, dict[tuple[str, str | None], tuple[Party, float]]] = {}  # session -> party -> (party, when)
        self._hands: dict[str, dict[int, list[str]]] = {}  # session -> hand -> who moved in it, in order
        self._current: dict[str, str] = {}  # account -> its table session id, once read or made

    # ---- the table -----------------------------------------------------------

    def lock(self, key: str) -> anyio.Lock:
        """Hold this across a step and its bookkeeping (key: a table session
        id), or across making an account's table (key: "account:<id>")."""
        lock = self._locks.get(key)
        if lock is None:
            lock = self._locks[key] = anyio.Lock()
        return lock

    async def current(self, account: str) -> str | None:
        """The account's table session id, or None before it has one. Read
        from the store once per account and process; this server is the
        only writer (_new), so it keeps it from there on."""
        if account in self._current:
            return self._current[account]
        index = await self.store.get("account-tables", account) or {"sessions": []}
        current = index["sessions"][0] if index["sessions"] else None
        if current is not None:
            self._current[account] = current
        return current

    async def ensure(self, account: str) -> tuple[str, dict]:
        """(table session id, its state): the account's table, made if it
        has none, or if the Go table no longer knows it (a session with no
        finished hand does not survive a table restart)."""
        async with self.lock("account:" + account):
            session_id = await self.current(account)
            if session_id is not None:
                try:
                    return session_id, await anyio.to_thread.run_sync(lambda: self.client.state(session_id))
                except TableError as e:
                    if e.status != 404:
                        raise
            return await self._new(account, seat="alternate", opponent=None)

    async def reset(self, account: str, seat: str, opponent: str | None) -> tuple[str, dict]:
        """A new table for the account: a new table session, hand 1, the
        score from zero, with these settings."""
        async with self.lock("account:" + account):
            return await self._new(account, seat=seat, opponent=opponent)

    async def _new(self, account: str, seat: str, opponent: str | None) -> tuple[str, dict]:
        state = await anyio.to_thread.run_sync(lambda: self.client.new(seat=seat, opponent=opponent))
        session_id = state["session"]
        await self.store.put("table-sessions", session_id, {"owner": account, "created_at": int(self.clock())})
        index = await self.store.get("account-tables", account) or {"sessions": []}
        index["sessions"] = [session_id, *[s for s in index["sessions"] if s != session_id]][:LIST_LIMIT]
        await self.store.put("account-tables", account, index)
        self._current[account] = session_id
        self.observe(session_id, state)
        return session_id, state

    async def resolve(self, account: str, session_id: object) -> str:
        """The table a request means: the account's own, named or not.
        LookupError for any other session id (another account's, an old one
        of this account's, one that does not exist: all alike) and when the
        account has no table yet."""
        current = await self.current(account)
        if current is None or (session_id not in (None, "") and session_id != current):
            raise LookupError("no such session")
        return current

    # ---- who is at the table -------------------------------------------------

    def moved(self, session_id: str, party: Party, before: dict) -> None:
        """`party` made the move (or dealt the hand) pending in `before`."""
        now = self.clock()
        self.seen(session_id, party)
        self._last[session_id] = {"kind": party.kind, "name": party.name, "at": now,
                                  "hand": before.get("hand"), "step": before.get("step")}
        hands = self._hands.setdefault(session_id, {})
        hand = before.get("hand")
        if hand is not None and not before.get("over"):
            who = hands.setdefault(hand, [])
            if party.name not in who:
                who.append(party.name)
            for old in sorted(hands)[:-HANDS_KEPT]:
                del hands[old]

    def seen(self, session_id: str, party: Party) -> None:
        self._seen.setdefault(session_id, {})[(party.kind, party.key)] = (party, self.clock())

    def last_mover(self, session_id: str) -> str | None:
        last = self._last.get(session_id)
        return last["name"] if last else None

    def presence(self, session_id: str) -> dict:
        """Who is at the table, for the page: `last` (the last move: kind,
        name, seconds ago, hand), `others` (the agent and programs seen,
        with seconds since), `hands` (hand number -> who moved in it)."""
        now = self.clock()
        last = self._last.get(session_id)
        others = [
            {"kind": p.kind, "name": p.name, "ago": round(now - at)}
            for p, at in self._seen.get(session_id, {}).values()
            if p.kind != BROWSER
        ]
        others.sort(key=lambda o: o["ago"])
        return {
            "last": {"kind": last["kind"], "name": last["name"], "ago": round(now - last["at"]), "hand": last["hand"]} if last else None,
            "others": others,
            "hands": {str(h): who for h, who in self._hands.get(session_id, {}).items()},
        }

    # ---- finished hands, for a page that polled past them --------------------

    def observe(self, session_id: str, state: dict) -> None:
        """Every table state that passes through this server (a move, a
        next hand, a page load): a finished hand's final state is kept (the
        latest two hands), so a page that polled past it can still be
        shown how it ended."""
        if state.get("over"):
            finals = self._finals.setdefault(session_id, {})
            finals[state["hand"]] = state
            for hand in sorted(finals)[:-2]:
                del finals[hand]

    def final_state(self, session_id: str, hand: int) -> dict | None:
        return self._finals.get(session_id, {}).get(hand)
