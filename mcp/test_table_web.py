"""The browser table at /table (table_web.py) and the control rules behind
it (tables.py), over real HTTP: our server in-process on a local socket,
signed in through a fake GitHub (oauth_testing.FakeIdP) so a browser and
an agent can be the same account, and the live table behind it (the
`site` fixture, conftest.py).

The table-touching tests skip themselves if the live table at
localhost:9777/poker/ is not reachable.

    python -m pytest mcp/test_table_web.py -q
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2
import pytest

import oauth_testing as ot
from conftest import USER_A, USER_B
from table_client import TableClient



def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


needs_table = pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")


def _payload(result):
    assert not result.is_error, result.content
    if result.structured_content is not None:
        return result.structured_content
    return json.loads(result.content[0].text)


def _pick(state):
    """Check or call if offered, so a hand goes on; else the first action."""
    kinds = {a["kind"]: a["index"] for a in state["actions"]}
    return kinds.get("check", kinds.get("call", state["actions"][0]["index"]))


async def _agent_to_a_live_decision(mcp):
    """new_session, then act until the agent faces a decision mid-hand
    (a new hand if one ends first). Returns that state."""
    s = _payload(await mcp.call_tool("new_session", {"opponent": "turtle"}))
    for _ in range(20):
        if not s["over"] and s["actions"]:
            return s
        s = _payload(await mcp.call_tool("next_hand", {}))
    raise AssertionError("no live decision")


@pytest.mark.asyncio
async def test_the_table_needs_sign_in_but_its_files_do_not(site):
    async with httpx2.AsyncClient(base_url=site.base, timeout=10) as http:
        for path in ("/table", "/table/", "/table/watch/sometoken"):
            r = await http.get(path)
            assert r.status_code == 303, (path, r.status_code)
            assert r.headers["location"].startswith("/table/signin?next="), r.headers["location"]
        r = await http.get("/table/api/state", params={"session": "x"})
        assert r.status_code == 401 and r.json()["signIn"].startswith("/table/signin")
        r = await http.post("/table/api/new", json={})
        assert r.status_code == 401
        # The sign-in refuses to send anyone off-site afterwards.
        r = await http.get("/table/signin", params={"next": "https://evil.example/"})
        assert r.status_code == 303
        assert "evil" not in r.cookies.get("obgl_table_signin", "")


@pytest.mark.asyncio
@needs_table
async def test_a_guest_browser_plays_a_hand(site):
    async with site.browser(None) as browser:
        page = await browser.get("/table/")
        assert page.status_code == 200 and '<base href="/table/">' in page.text
        assert page.headers["referrer-policy"] == "no-referrer"
        assert (await browser.get("/table/table.js")).status_code == 200
        info = (await browser.get("/table/api/info")).json()
        assert info["league"]["account"] == {"name": "Guest", "guest": True}
        s = (await browser.post("/table/api/new", json={"seat": "alternate", "opponent": "turtle"})).json()
        assert s["control"]["yours"] and not s["control"]["canTagIn"]
        for _ in range(50):
            if s["over"]:
                break
            r = await browser.post("/table/api/act", json={"session": s["session"], "hand": s["hand"], "step": s["step"], "action": _pick(s)})
            assert r.status_code == 200, r.text
            s = r.json()
        assert s["over"]
        r = await browser.post("/table/api/next", json={"session": s["session"], "hand": s["hand"]})
        assert r.status_code == 200 and r.json()["hand"] == s["hand"] + 1
        # A guest's agent is another guest: no tagging in.
        r = await browser.post("/table/api/tagin", json={"session": s["session"]})
        assert r.status_code == 400 and "guest" in r.json()["error"]
        # A request from another origin is refused, signed in or not.
        r = await browser.post("/table/api/new", json={}, headers={"Origin": "https://evil.example"})
        assert r.status_code == 403


@pytest.mark.asyncio
@needs_table
async def test_watch_is_for_the_owner_only(site):
    token = await site.agent_token(USER_A)
    async with ot.mcp_client(f"{site.base}/mcp", token) as mcp:
        s = _payload(await mcp.call_tool("new_session", {"opponent": "turtle"}))
        watch_url = s["watch_url"]
        assert watch_url.startswith(f"{site.base}/table/watch/")
        watch_token = watch_url.rsplit("/", 1)[1]
        assert watch_token != s["session"]
        assert _payload(await mcp.call_tool("score", {}))["watch_url"] == watch_url

    async with site.browser(USER_A) as owner:
        page = await owner.get(f"/table/watch/{watch_token}")
        assert page.status_code == 200 and '<base href="/table/">' in page.text
        w = (await owner.get("/table/api/watch", params={"token": watch_token})).json()
        assert "session" not in w  # a watcher never gets the id that acts
        assert w["hand"] == s["hand"] and w["you"]["cards"] == s["you"]["cards"]
        assert "cards" not in w["bot"] or w["over"]  # the bot's cards only when shown
        assert w["control"]["controller"] == "agent" and not w["control"]["yours"]
        assert w.get("history", []) == []  # the table leaves an empty history out

    for who in (USER_B, None):  # another account, a guest
        async with site.browser(who) as other:
            assert (await other.get(f"/table/watch/{watch_token}")).status_code == 404
            r = await other.get("/table/api/watch", params={"token": watch_token})
            assert r.status_code == 404 and r.json() == {"error": "no such session"}
            assert (await other.post("/table/api/takeover", json={"token": watch_token})).status_code == 404
            # Nor by the session id, had it leaked.
            assert (await other.get("/table/api/state", params={"session": s["session"]})).status_code == 404
            r = await other.post("/table/api/act", json={"session": s["session"], "hand": s["hand"], "step": s["step"], "action": _pick(s)})
            assert r.status_code == 404
        # ... and as an agent: not in its list, and it cannot claim it.
        async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(who)) as mcp:
            listed = _payload(await mcp.call_tool("my_sessions", {}))["sessions"]
            assert s["session"] not in [x["session"] for x in listed]
            assert "no such session" in _payload(await mcp.call_tool("claim_session", {"session": s["session"]}))["error"]

    # A guest's agent gets no watch link (no browser can ever be that guest).
    async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(None)) as mcp:
        g = _payload(await mcp.call_tool("new_session", {}))
        assert g["watch_url"] is None and "guest" in g["watch_note"]


@pytest.mark.asyncio
@needs_table
async def test_a_watch_token_cannot_act(site):
    async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        s = await _agent_to_a_live_decision(mcp)
        watch_token = s["watch_url"].rsplit("/", 1)[1]
        async with site.browser(USER_A) as owner:
            move = {"hand": s["hand"], "step": s["step"], "action": _pick(s)}
            for path, body in (
                ("/table/api/act", {"session": watch_token, **move}),
                ("/table/api/act", {"token": watch_token, **move}),
                ("/table/api/next", {"session": watch_token, "hand": s["hand"]}),
                ("/table/api/tagin", {"session": watch_token}),
            ):
                r = await owner.post(path, json=body)
                assert r.status_code == 404, (path, r.status_code, r.text)
            assert (await owner.get("/table/api/state", params={"session": watch_token})).status_code == 404
            # The owner's browser, with the real id, still cannot act while the agent plays.
            r = await owner.post("/table/api/act", json={"session": s["session"], **move})
            assert r.status_code == 409 and r.json()["control"]["controller"] == "agent"
        # Nothing moved: the agent's decision is still pending, and it is the agent's.
        after = _payload(await mcp.call_tool("act", {"action": _pick(s)}))
        assert "error" not in after, after
        assert (after["hand"], after["step"]) != (s["hand"], s["step"])


@pytest.mark.asyncio
@needs_table
async def test_take_over_then_tag_in_hands_the_pending_decision_across(site):
    async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as first:
        s = await _agent_to_a_live_decision(first)
        session, watch_token = s["session"], s["watch_url"].rsplit("/", 1)[1]
        async with site.browser(USER_A) as owner:
            # Watch -> Play, mid-decision: the browser inherits exactly that decision.
            t = (await owner.post("/table/api/takeover", json={"token": watch_token})).json()
            assert t["session"] == session and (t["hand"], t["step"]) == (s["hand"], s["step"])
            assert t["actions"] == s["actions"] and t["control"]["yours"]
            refused = _payload(await first.call_tool("act", {"action": _pick(s)}))
            assert "not playing this session" in refused["error"] and "claim_session" in refused["error"]
            assert "browser" in _payload(await first.call_tool("state", {}))["control"]
            assert "not playing" in _payload(await first.call_tool("next_hand", {}))["error"]
            r = await owner.post("/table/api/act", json={"session": session, "hand": t["hand"], "step": t["step"], "action": _pick(t)})
            assert r.status_code == 200, r.text
            b = r.json()
            assert (b["hand"], b["step"]) != (t["hand"], t["step"])
            while b["over"]:  # get to a live decision again, in the browser
                b = (await owner.post("/table/api/next", json={"session": session, "hand": b["hand"]})).json()

            # Play -> agent: the browser drops to Watch; nobody acts until an agent claims it.
            g = (await owner.post("/table/api/tagin", json={"session": session})).json()
            assert g["control"]["waiting"] and "session" not in g and g["control"]["watch"] == f"watch/{watch_token}"
            r = await owner.post("/table/api/act", json={"session": session, "hand": b["hand"], "step": b["step"], "action": _pick(b)})
            assert r.status_code == 409
            assert "not playing" in _payload(await first.call_tool("act", {"action": _pick(b)}))["error"]

            # The owner's agent, on a new connection, finds it and claims it.
            async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as second:
                listed = _payload(await second.call_tool("my_sessions", {}))["sessions"]
                mine = next(x for x in listed if x["session"] == session)
                assert mine["playing"].startswith("nobody") and not mine["yours"]
                assert mine["watch_url"] == s["watch_url"]
                c = _payload(await second.call_tool("claim_session", {"session": session}))
                assert (c["hand"], c["step"]) == (b["hand"], b["step"]) and c["actions"] == b["actions"]
                d = _payload(await second.call_tool("act", {"action": _pick(c)}))
                assert "error" not in d, d
                # The first connection is still out; the browser watches it land.
                assert "not playing" in _payload(await first.call_tool("act", {"action": _pick(d) if d["actions"] else 0}))["error"]
                w = (await owner.get("/table/api/watch", params={"token": watch_token, "brief": "1", "hand": c["hand"]})).json()
                assert (w["hand"], w["step"]) == (d["hand"], d["step"])
                switches = [(x["from"], x["to"], x["by"]) for x in w["control"]["switches"]]
                assert switches == [
                    ("agent", "browser", "browser"),
                    ("browser", "unclaimed", "browser"),
                    ("unclaimed", "agent", "agent"),
                ]
                assert (w["control"]["switches"][0]["hand"], w["control"]["switches"][0]["step"]) == (s["hand"], s["step"])


@pytest.mark.asyncio
@needs_table
async def test_a_watcher_behind_a_finished_hand_is_shown_its_end(site):
    async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        s = await _agent_to_a_live_decision(mcp)
        watch_token = s["watch_url"].rsplit("/", 1)[1]
        hand = s["hand"]
        while s["hand"] == hand and not s["over"]:
            s = _payload(await mcp.call_tool("act", {"action": _pick(s)}))
        final = s
        s = _payload(await mcp.call_tool("next_hand", {}))
        async with site.browser(USER_A) as owner:
            # The page last saw `hand` unfinished: it gets that hand's end first ...
            w = (await owner.get("/table/api/watch", params={"token": watch_token, "brief": "1", "hand": hand, "over": "0"})).json()
            assert w["catchUp"] and w["hand"] == hand and w["over"] and w["net"] == final["net"]
            # ... then, having shown it, the hand in play, with the history to fill in.
            w = (await owner.get("/table/api/watch", params={"token": watch_token, "brief": "1", "hand": hand, "over": "1"})).json()
            assert "catchUp" not in w and w["hand"] == s["hand"] and hand in [h["number"] for h in w["history"]]
            # Nothing new: no history resent.
            w = (await owner.get("/table/api/watch", params={"token": watch_token, "brief": "1", "hand": s["hand"]})).json()
            assert "history" not in w


@pytest.mark.asyncio
async def test_the_browser_stays_signed_in_through_a_refresh_and_signs_out(site):
    async with site.browser(USER_A) as browser:
        assert (await browser.get("/table/api/info")).json()["league"]["account"]["name"] == "alice"
        old_refresh = browser.cookies.get("obgl_table_rt")
        # The access token runs out (here: is gone): the refresh token signs the
        # browser in again, as the same account, and both cookies rotate.
        browser.cookies.set("obgl_table_at", "obgl_at_expired", domain="127.0.0.1", path="/table")
        r = await browser.get("/table/api/info")
        assert r.json()["league"]["account"]["name"] == "alice"
        assert "obgl_table_at=" in r.headers.get("set-cookie", "")
        assert browser.cookies.get("obgl_table_rt") != old_refresh
        # Sign out: the tokens are revoked and the cookies cleared.
        tokens = (browser.cookies.get("obgl_table_at"), browser.cookies.get("obgl_table_rt"))
        assert (await browser.post("/table/signout", json={})).status_code == 200
        assert (await browser.get("/table/api/info")).json()["league"]["account"] is None
        browser.cookies.set("obgl_table_at", tokens[0], domain="127.0.0.1", path="/table")
        browser.cookies.set("obgl_table_rt", tokens[1], domain="127.0.0.1", path="/table")
        assert (await browser.get("/table/api/info")).json()["league"]["account"] is None
