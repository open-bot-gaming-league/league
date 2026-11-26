"""Session bookkeeping on top of TableClient: the hand/step fill-in,
staleness checks, and score computation. No MCP or network-framing lives
here, so it is unit-testable with a fake client.
"""

from __future__ import annotations

from typing import Any

from table_client import TableClient, TableError


class NoSessionError(Exception):
    """No session is open yet."""


class StaleStateError(Exception):
    """The cached hand/step no longer matches what act()/next_hand() needs."""


class MovedOnError(StaleStateError):
    """The table refused a move because the decision it was meant for has
    already been made (409): with one table played by whoever moves
    first, usually by someone else at it (the user, or their program)."""


class InvalidActionError(Exception):
    """The requested action is not one the current state allows."""


# act()'s move kinds (API.md "Bet sizes"); bet and raise are one move.
MOVE_KINDS = ("fold", "check", "call", "bet", "raise", "allin")


# Keys from a table state we keep in the compact view handed to the agent.
# Dropped: events (animation only -- the wrapper is turn-based, not a
# page to animate), totals/against (score() recomputes them from history
# instead), finished (outcome/net cover it), history (score() pulls it
# fresh). `session` is kept despite being otherwise unused by the agent:
# it is the one way to cross-check the wrapper's own bookkeeping against
# the table's api/state directly, and it costs a few tokens.
_STATE_VIEW_KEYS = (
    "session", "hand", "step", "round", "seat", "opponent",
    "you", "bot", "board", "pot", "over", "actions", "legal", "lines",
)


def summarize_state(state: dict) -> dict:
    """The compact view of a table state returned by every poker tool."""
    out = {k: state[k] for k in _STATE_VIEW_KEYS if k in state}
    if state.get("over"):
        out["outcome"] = state.get("outcome")
        out["net"] = state.get("net")
    return out


def compute_score(history: list[dict], big_blind: int) -> dict:
    """hands / net bb / bb per 100, overall and per opponent, from a
    session's `history` (as returned by api/state). Pure function: no
    network, so it is the thing the unit tests exercise directly.
    """
    bb = big_blind or 1
    total_hands = len(history)
    total_net_chips = sum(h["net"] for h in history)
    total_net_bb = total_net_chips / bb
    total_bb_per100 = (total_net_bb / total_hands * 100) if total_hands else 0.0

    by_opp: dict[str, dict[str, Any]] = {}
    for h in history:
        opp = h.get("opponent", "?")
        rec = by_opp.setdefault(opp, {"hands": 0, "net_chips": 0})
        rec["hands"] += 1
        rec["net_chips"] += h["net"]

    by_opponent = {}
    for opp, rec in by_opp.items():
        net_bb = rec["net_chips"] / bb
        bb_per100 = (net_bb / rec["hands"] * 100) if rec["hands"] else 0.0
        by_opponent[opp] = {
            "hands": rec["hands"],
            "net_chips": rec["net_chips"],
            "net_bb": round(net_bb, 2),
            "bb_per100": round(bb_per100, 2),
        }

    return {
        "hands": total_hands,
        "net_chips": total_net_chips,
        "net_bb": round(total_net_bb, 2),
        "bb_per100": round(total_bb_per100, 2),
        "by_opponent": by_opponent,
    }


class PokerSession:
    """One stranger's play against the table: tracks the session id and
    the last state seen, so the MCP tools can stay stateless-looking to
    the agent (no session/hand/step arguments to get wrong).
    """

    def __init__(self, client: TableClient):
        self.client = client
        self.session_id: str | None = None
        self.big_blind: int | None = None
        self.last_state: dict | None = None

    def _require_session(self) -> None:
        if not self.session_id or self.last_state is None:
            raise NoSessionError("no active session: call table() first")

    def new(self, opponent: str | None = None, seat: str | None = None) -> dict:
        info = self.client.info()
        self.big_blind = info["bigBlind"]
        chosen_opponent = opponent or info["bots"][0]["id"]
        state = self.client.new(seat=seat or "alternate", opponent=chosen_opponent)
        self.session_id = state["session"]
        self.last_state = state
        return state

    def attach(self, session_id: str) -> dict:
        """Play an existing table session from here on (with sign-in: the
        account's one table). The state is fetched fresh, so the decision
        pending in it is the one act() makes next."""
        info = self.client.info()
        state = self.client.state(session_id)
        self.big_blind = info["bigBlind"]
        self.session_id = session_id
        self.last_state = state
        return state

    def state(self) -> dict:
        self._require_session()
        state = self.client.state(self.session_id)
        self.last_state = state
        return state

    def act(self, action: int | str, amount: int | None = None) -> dict:
        """action: a move kind (MOVE_KINDS) with `amount` for a bet or
        raise, or (the old form) a button's index from `actions`. The
        table checks the amount against the state's `legal`; a refusal
        comes back as InvalidActionError with the table's reason."""
        self._require_session()
        assert self.last_state is not None
        if self.last_state.get("over"):
            raise StaleStateError(
                "the current hand is over: call next_hand() before acting again"
            )
        if isinstance(action, str) and action.strip().isdigit():
            action = int(action)  # an index sent as text
        form: dict[str, Any]
        if isinstance(action, bool) or not isinstance(action, (int, str)):
            raise InvalidActionError(f"action must be a kind ({', '.join(MOVE_KINDS)}) or an index, not {action!r}")
        if isinstance(action, int):
            valid = {a["index"] for a in self.last_state.get("actions", [])}
            if action not in valid:
                raise InvalidActionError(
                    f"action {action} is not one of the current legal actions "
                    f"{sorted(valid)}; call state() if you are not sure what is legal"
                )
            if amount is not None:
                raise InvalidActionError(
                    "an index already says the amount: send act(index) alone, "
                    "or act(kind, amount) for any size"
                )
            form = {"action": action}
        else:
            kind = action.strip().lower().replace("-", "").replace(" ", "")
            if kind not in MOVE_KINDS:
                raise InvalidActionError(
                    f"action {action!r} is not a move: use one of {', '.join(MOVE_KINDS)} "
                    "(with amount for bet/raise), or an index from actions"
                )
            form = {"kind": kind, "amount": amount}
        try:
            state = self.client.act(
                self.session_id, self.last_state["hand"], self.last_state["step"], **form
            )
        except TableError as e:
            if e.status == 409:
                raise MovedOnError(
                    f"stale: {e.message}; call state() to refresh, then act() again"
                ) from e
            if e.status == 400:
                raise InvalidActionError(
                    f"{e.message}; see legal in the last state (call state() if unsure)"
                ) from e
            raise
        self.last_state = state
        return state

    def next_hand(self) -> dict:
        self._require_session()
        assert self.last_state is not None
        if not self.last_state.get("over"):
            raise StaleStateError(
                "the current hand is not over yet: finish it with act() first"
            )
        try:
            state = self.client.next(self.session_id, self.last_state["hand"])
        except TableError as e:
            if e.status == 409:
                raise MovedOnError(
                    f"stale: {e.message}; call state() to refresh"
                ) from e
            raise
        self.last_state = state
        return state

    def score(self) -> dict:
        self._require_session()
        state = self.client.state(self.session_id)
        self.last_state = state
        history = state.get("history", [])
        return compute_score(history, self.big_blind or 100)
