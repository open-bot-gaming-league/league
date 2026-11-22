"""Unit tests for the hand/step bookkeeping and score() in session.py.
No network: TableClient is replaced by a small fake that scripts a few
hands' worth of table responses.

Run: python -m pytest mcp/test_session.py
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from session import (
    InvalidActionError,
    NoSessionError,
    PokerSession,
    StaleStateError,
    compute_score,
    summarize_state,
)
from table_client import TableError


def _state(hand, step, over=False, actions=None, net=None, outcome=None, opponent="bot-a"):
    s = {
        "session": "sess-1",
        "hand": hand,
        "step": step,
        "round": "before the flop",
        "seat": "big blind",
        "opponent": opponent,
        "you": {"cards": ["As", "Kd"], "stack": 4900, "put": 100},
        "bot": {"stack": 4900, "put": 100},
        "board": [],
        "pot": 200,
        "over": over,
        "actions": actions or [],
        "lines": [],
        "events": [{"seat": "you", "round": 0, "kind": "x"}],  # dropped by summarize_state
    }
    if over:
        s["outcome"] = outcome or "You folded and lost 100"
        s["net"] = net if net is not None else -100
    return s


class FakeClient:
    """Scripts a fixed sequence of responses; records every call made."""

    def __init__(self, info, script):
        self._info = info
        self._script = list(script)  # list of (method_name, response_or_exception)
        self.calls = []
        self.acts = []  # every act() call's arguments

    def _next(self, name):
        self.calls.append(name)
        want_name, result = self._script.pop(0)
        assert want_name == name, f"expected a call to {want_name}, got {name}"
        if isinstance(result, Exception):
            raise result
        return result

    def info(self):
        self.calls.append("info")
        return self._info

    def new(self, seat, opponent):
        return self._next("new")

    def state(self, session):
        return self._next("state")

    def act(self, session, hand, step, action=None, kind=None, amount=None):
        self.acts.append({"hand": hand, "step": step, "action": action, "kind": kind, "amount": amount})
        return self._next("act")

    def next(self, session, hand, seat=None, opponent=None):
        return self._next("next")


INFO = {
    "bots": [{"id": "bot-a", "name": "Bot A"}],
    "stackBB": 50,
    "smallBlind": 50,
    "bigBlind": 100,
    "menu": "...",
}


# -- summarize_state ---------------------------------------------------

def test_summarize_state_drops_heavy_fields_and_keeps_core_ones():
    s = _state(3, 1, actions=[{"index": 0, "label": "Check", "kind": "check"}])
    view = summarize_state(s)
    assert view["hand"] == 3 and view["step"] == 1
    assert view["actions"] == [{"index": 0, "label": "Check", "kind": "check"}]
    assert "events" not in view
    assert "outcome" not in view and "net" not in view  # hand not over


def test_summarize_state_includes_outcome_and_net_once_over():
    s = _state(3, 4, over=True, net=-100, outcome="You folded and lost 100")
    view = summarize_state(s)
    assert view["over"] is True
    assert view["outcome"] == "You folded and lost 100"
    assert view["net"] == -100


# -- PokerSession: no session yet --------------------------------------

def test_act_before_new_session_raises_no_session():
    poker = PokerSession(FakeClient(INFO, []))
    with pytest.raises(NoSessionError):
        poker.act(0)


def test_score_before_new_session_raises_no_session():
    poker = PokerSession(FakeClient(INFO, []))
    with pytest.raises(NoSessionError):
        poker.score()


# -- PokerSession: hand/step fill-in -------------------------------------

def test_new_session_captures_session_id_and_big_blind():
    s1 = _state(1, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    poker = PokerSession(FakeClient(INFO, [("new", s1)]))
    poker.new()
    assert poker.session_id == "sess-1"
    assert poker.big_blind == 100
    assert poker.last_state is s1


def test_act_fills_in_hand_and_step_from_last_state():
    s1 = _state(1, 2, actions=[{"index": 1, "label": "Call 100", "kind": "call"}])
    s2 = _state(1, 3, over=True, net=-100)
    client = FakeClient(INFO, [("new", s1), ("act", s2)])
    poker = PokerSession(client)
    poker.new()
    poker.act(1)
    # the client never saw hand/step from the caller -- act() supplied them
    assert client.calls[-1] == "act"
    assert poker.last_state["step"] == 3


def test_act_rejects_an_action_not_in_the_current_menu():
    s1 = _state(1, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    poker = PokerSession(FakeClient(INFO, [("new", s1)]))
    poker.new()
    with pytest.raises(InvalidActionError):
        poker.act(5)


def test_act_after_hand_over_raises_stale_telling_to_call_next_hand():
    s1 = _state(1, 3, over=True, net=100)
    poker = PokerSession(FakeClient(INFO, [("new", s1)]))
    poker.new()
    with pytest.raises(StaleStateError, match="next_hand"):
        poker.act(0)


def test_act_409_from_table_becomes_a_clear_stale_error():
    s1 = _state(1, 1, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    err = TableError(409, "the hand has moved on since that was sent")
    client = FakeClient(INFO, [("new", s1), ("act", err)])
    poker = PokerSession(client)
    poker.new()
    with pytest.raises(StaleStateError, match="moved on"):
        poker.act(0)


def test_next_hand_rejects_when_current_hand_is_not_over():
    s1 = _state(1, 1, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    poker = PokerSession(FakeClient(INFO, [("new", s1)]))
    poker.new()
    with pytest.raises(StaleStateError, match="not over"):
        poker.next_hand()


def test_next_hand_passes_the_finished_hand_number():
    s1 = _state(1, 3, over=True, net=-50)
    s2 = _state(2, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    client = FakeClient(INFO, [("new", s1), ("next", s2)])
    poker = PokerSession(client)
    poker.new()
    poker.next_hand()
    assert poker.last_state["hand"] == 2


def test_state_refreshes_the_cached_hand_and_step():
    s1 = _state(1, 1, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    s1_refreshed = _state(1, 2, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    client = FakeClient(INFO, [("new", s1), ("state", s1_refreshed)])
    poker = PokerSession(client)
    poker.new()
    poker.state()
    assert poker.last_state["step"] == 2


# -- compute_score --------------------------------------------------------

def test_compute_score_empty_history():
    out = compute_score([], 100)
    assert out == {"hands": 0, "net_chips": 0, "net_bb": 0.0, "bb_per100": 0.0, "by_opponent": {}}


def test_compute_score_overall_and_per_opponent():
    history = [
        {"opponent": "bot-a", "net": -100},
        {"opponent": "bot-a", "net": 300},
        {"opponent": "bot-b", "net": -200},
    ]
    out = compute_score(history, 100)
    assert out["hands"] == 3
    assert out["net_chips"] == 0
    assert out["net_bb"] == 0.0
    assert out["bb_per100"] == 0.0
    assert out["by_opponent"]["bot-a"] == {
        "hands": 2, "net_chips": 200, "net_bb": 2.0, "bb_per100": 100.0,
    }
    assert out["by_opponent"]["bot-b"] == {
        "hands": 1, "net_chips": -200, "net_bb": -2.0, "bb_per100": -200.0,
    }


def test_score_pulls_a_fresh_state_and_computes_from_its_history():
    s1 = _state(1, 1, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    fresh = _state(2, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    fresh["history"] = [
        {"opponent": "bot-a", "net": -100},
        {"opponent": "bot-a", "net": -100},
    ]
    client = FakeClient(INFO, [("new", s1), ("state", fresh)])
    poker = PokerSession(client)
    poker.new()
    out = poker.score()
    assert out["hands"] == 2
    assert out["net_bb"] == -2.0
    assert out["bb_per100"] == -100.0


# -- act(): kind + amount, and the old index form ------------------------

_LEGAL = {"fold": True, "check": False, "call": 100, "wager": "raise", "minTo": 300, "maxTo": 5000}


def _poker_at(state, *more):
    client = FakeClient(INFO, [("new", state), *more])
    poker = PokerSession(client)
    poker.new()
    return poker, client


def test_summarize_state_keeps_legal():
    s = _state(1, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    s["legal"] = _LEGAL
    assert summarize_state(s)["legal"] == _LEGAL


def test_act_sends_kind_and_amount():
    s1 = _state(1, 2, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    poker, client = _poker_at(s1, ("act", _state(1, 4)), ("act", _state(1, 6)))
    poker.act("raise", 650)
    assert client.acts[-1] == {"hand": 1, "step": 2, "action": None, "kind": "raise", "amount": 650}
    poker.act(" All-In ")  # spelled loosely: still the allin kind
    assert client.acts[-1]["kind"] == "allin" and client.acts[-1]["amount"] is None
    assert client.acts[-1]["step"] == 4  # from the state the first act returned


def test_act_old_index_form_still_sends_action():
    s1 = _state(1, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}, {"index": 1, "label": "Call 50", "kind": "call"}])
    poker, client = _poker_at(s1, ("act", _state(1, 1)), ("act", _state(1, 2)))
    poker.act(1)
    assert client.acts[-1] == {"hand": 1, "step": 0, "action": 1, "kind": None, "amount": None}
    poker.last_state = s1
    poker.act("0")  # an index sent as text
    assert client.acts[-1]["action"] == 0


@pytest.mark.parametrize("action,amount", [("shove", None), (0, 300), (True, None), (None, None)])
def test_act_refuses_what_is_not_a_move(action, amount):
    s1 = _state(1, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    poker, client = _poker_at(s1)
    with pytest.raises(InvalidActionError):
        poker.act(action, amount)
    assert client.acts == []  # refused before the table was asked


def test_act_400_from_table_becomes_invalid_action_with_the_reason():
    s1 = _state(1, 0, actions=[{"index": 0, "label": "Fold", "kind": "fold"}])
    err = TableError(400, "a wager here goes to 300-5000 chips (all-in 5000), not 250")
    poker, _ = _poker_at(s1, ("act", err))
    with pytest.raises(InvalidActionError, match="300-5000"):
        poker.act("raise", 250)
    assert poker.last_state is s1  # a refused move leaves the cached state alone
