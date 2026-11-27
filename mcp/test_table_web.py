"""The browser table at /table (table_web.py), each account's one table
(tables.py) and program tokens (programs.py), over real HTTP: our server
in-process on a local socket, signed in through a fake GitHub
(oauth_testing.FakeIdP) so a browser and an agent can be the same account,
and the live table behind it (the `site` fixture, conftest.py).

The table-touching tests skip themselves if the live table at
localhost:9777/poker/ is not reachable.

    python -m pytest mcp/test_table_web.py -q
"""

import asyncio
import base64
import json
import os
import sys
from urllib.parse import parse_qs, urlparse

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


async def _to_a_decision(get_state, act, deal):
    """From a state, check/call (act) and deal until a decision is pending."""
    s = get_state
    for _ in range(40):
        if not s["over"] and s.get("legal"):
            return s
        s = await deal(s) if s["over"] else await act(s)
    raise AssertionError("no live decision")


def _move(s):
    """Check or call, as the {kind} form."""
    return {"kind": "check" if s["legal"]["check"] else "call"}


class Page:
    """The table's API as the page (or a program) calls it."""

    def __init__(self, http, headers=None):
        self.http, self.headers = http, headers or {}

    async def get(self, path, **params):
        return await self.http.get("/table/api/" + path, params=params, headers=self.headers)

    async def post(self, path, body):
        return await self.http.post("/table/api/" + path, json=body, headers=self.headers)

    async def state(self, **params):
        r = await self.get("state", **params)
        assert r.status_code == 200, r.text
        return r.json()

    async def decision(self):
        async def act(s):
            r = await self.post("act", {"hand": s["hand"], "step": s["step"], **_move(s)})
            assert r.status_code == 200, r.text
            return r.json()

        async def deal(s):
            r = await self.post("next", {"hand": s["hand"]})
            assert r.status_code == 200, r.text
            return r.json()

        return await _to_a_decision(await self.state(), act, deal)


async def _agent_decision(mcp):
    s = _payload(await mcp.call_tool("table", {}))

    async def act(s):
        return _payload(await mcp.call_tool("act", {"action": _move(s)["kind"]}))

    async def deal(s):
        return _payload(await mcp.call_tool("next_hand", {}))

    return await _to_a_decision(s, act, deal)


@pytest.mark.asyncio
@needs_table
async def test_one_table_for_the_browser_and_every_agent_connection(site):
    async with site.browser(USER_A) as browser:
        page = await browser.get("/table/")
        assert page.status_code == 200 and '<base href="/table/">' in page.text
        assert page.headers["referrer-policy"] == "no-referrer"
        assert (await browser.get("/table/table.js")).status_code == 200
        info = (await browser.get("/table/api/info")).json()
        assert info["league"]["account"] == {"name": "alice"}
        assert info["league"]["ranked"] is False  # no bot submissions here: no ranked tables for the page to show
        p = Page(browser)
        first = await p.state()  # the table is made on first sight
        assert first["session"] and "presence" in first and "control" not in first
        assert (await p.state())["session"] == first["session"]  # ... and kept
        assert (await p.state(session=first["session"]))["session"] == first["session"]
        token = await site.agent_token(USER_A)
        async with ot.mcp_client(f"{site.base}/mcp", token) as one, ot.mcp_client(f"{site.base}/mcp", token) as two:
            t1, t2 = _payload(await one.call_tool("table", {})), _payload(await two.call_tool("table", {}))
            assert t1["session"] == t2["session"] == first["session"]
            assert t1["table_url"] == f"{site.base}/table/" and t1["seatMode"] == first["seatMode"]
            # table() is the one way: the old name is gone.
            tools = {t.name for t in (await one.list_tools()).tools}
            assert "table" in tools and "new_session" not in tools
            assert _payload(await one.call_tool("score", {}))["table_url"] == t1["table_url"]
        # A request from another origin is refused, signed in or not.
        r = await browser.post("/table/api/act", json={}, headers={"Origin": "https://evil.example"})
        assert r.status_code == 403
        # The page plays a whole hand on it.
        s = await p.decision()
        while not s["over"]:
            r = await p.post("act", {"hand": s["hand"], "step": s["step"], **_move(s)})
            assert r.status_code == 200, r.text
            s = r.json()
        r = await p.post("next", {"hand": s["hand"]})
        assert r.status_code == 200 and r.json()["hand"] == s["hand"] + 1
        assert r.json()["presence"]["last"]["name"] == "you"


@pytest.mark.asyncio
@needs_table
async def test_whoever_moves_first_makes_the_decision(site):
    async with site.browser(USER_A) as browser, ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        p = Page(browser)
        s = await _agent_decision(mcp)
        # The browser makes the agent's pending decision first ...
        r = await p.post("act", {"hand": s["hand"], "step": s["step"], **_move(s)})
        assert r.status_code == 200, r.text
        b = r.json()
        assert b["presence"]["last"]["name"] == "you"
        # ... so the agent's move for it is not made, and it is told so in words, with the new state.
        after = _payload(await mcp.call_tool("act", {"action": _move(s)["kind"]}))
        assert "error" not in after, after
        assert after["note"].startswith("you already acted on this decision"), after["note"]
        assert (after["hand"], after["step"]) == (b["hand"], b["step"])
        # Second to move, the browser gets the table's 409; the page reloads the state.
        s = await _agent_decision(mcp)
        moved = _payload(await mcp.call_tool("act", {"action": _move(s)["kind"]}))
        assert "error" not in moved and "note" not in moved, moved
        r = await p.post("act", {"hand": s["hand"], "step": s["step"], **_move(s)})
        assert r.status_code == 409
        assert (await p.state())["presence"]["last"]["name"] == "your agent"


@pytest.mark.asyncio
@needs_table
async def test_settings_are_the_pages_and_reset_is_the_pages(site):
    async with site.browser(USER_A) as browser, ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        p = Page(browser)
        s = await p.state()
        refused = _payload(await mcp.call_tool("table", {"opponent": "ox" if s["opponent"] != "ox" else "turtle"}))
        assert "settings are changed on the table page" in refused["error"], refused
        same = _payload(await mcp.call_tool("table", {"opponent": s["opponent"], "seat": s["seatMode"]}))
        assert same["session"] == s["session"]  # asking for what it already is: fine
        # The page resets: a new table, the score from zero; the agent is told when it next looks.
        r = await p.post("new", {"seat": "bb", "opponent": s["opponent"]})
        assert r.status_code == 200, r.text
        fresh = r.json()
        assert fresh["session"] != s["session"] and fresh["seatMode"] == "bb" and fresh["hand"] == 1
        looked = _payload(await mcp.call_tool("state", {}))
        assert looked["session"] == fresh["session"] and "reset the table" in looked["note"]
        assert (await p.get("state", session=s["session"])).status_code == 404  # the old one is nobody's table now


@pytest.mark.asyncio
@needs_table
async def test_a_table_is_its_owners_only(site):
    async with site.browser(USER_A) as alice:
        s = await Page(alice).state()
    async with site.browser(USER_B) as bob:
        p = Page(bob)
        mine = await p.state()
        assert mine["session"] != s["session"]
        assert (await p.get("state", session=s["session"])).status_code == 404
        for path, body in (("act", {"session": s["session"], "hand": s["hand"], "step": s["step"], "kind": "call"}),
                           ("next", {"session": s["session"], "hand": s["hand"]})):
            r = await p.post(path, body)
            assert r.status_code == 404 and r.json() == {"error": "no such session"}, (path, r.text)


@pytest.mark.asyncio
@needs_table
async def test_a_program_token_plays_its_owners_table_and_nothing_else(site):
    async with site.browser(USER_A) as browser:
        page = Page(browser)
        table = await page.state()
        assert (await page.get("token")).json() == {"token": None}
        r = await page.post("token", {"label": "my bot"})
        assert r.status_code == 200, r.text
        made = r.json()
        token = made["token"]
        assert token.startswith("obgl_pt_") and made["label"] == "my bot"
        shown = (await page.get("token")).json()["token"]
        assert shown["id"] == made["id"] and shown["label"] == "my bot" and token not in repr(shown)

        async with httpx2.AsyncClient(base_url=site.base, timeout=30) as http:
            prog = Page(http, {"Authorization": f"Bearer {token}"})
            s = await prog.decision()
            assert s["session"] == table["session"]
            r = await prog.post("act", {"hand": s["hand"], "step": s["step"], **_move(s)})
            assert r.status_code == 200, r.text
            assert r.json()["presence"]["last"]["name"] == "your program \u201cmy bot\u201d"
            seen = (await page.state())["presence"]
            assert any(o["kind"] == "program" for o in seen["others"])
            # What the page alone may do.
            for path, body in (("new", {}), ("token", {"label": "x"}), ("token/regenerate", {}), ("token/revoke", {})):
                r = await prog.post(path, body)
                assert r.status_code == 403, (path, r.status_code)
            assert (await prog.get("token")).status_code == 403
            s = await prog.state()
            while not s["over"]:
                s = (await prog.post("act", {"hand": s["hand"], "step": s["step"], **_move(s)})).json()
            r = await prog.post("next", {"hand": s["hand"], "opponent": "ox"})
            assert r.status_code == 403 and "settings are changed on the table page" in r.json()["error"]
            assert (await prog.post("next", {"hand": s["hand"]})).status_code == 200
            # Not an MCP token, and not a key to anything else.
            r = await http.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
                                headers={"Authorization": f"Bearer {token}", "Accept": "application/json, text/event-stream"})
            assert r.status_code == 401
            # A bad token is a 401 even with the owner's cookies: the header decides.
            r = await browser.get("/table/api/state", headers={"Authorization": "Bearer obgl_pt_nope"})
            assert r.status_code == 401 and "program token" in r.json()["error"]
            # An agent's access token is not a program token either.
            agent = await site.agent_token(USER_A)
            assert (await http.get("/table/api/state", headers={"Authorization": f"Bearer {agent}"})).status_code == 401
            # Revoked: refused at once.
            assert (await page.post("token/revoke", {})).json() == {"revoked": True}
            assert (await prog.get("state")).status_code == 401
            assert (await page.get("token")).json() == {"token": None}


@pytest.mark.asyncio
@needs_table
async def test_a_program_token_is_rate_limited(site):
    async with site.browser(USER_A) as browser:
        token = (await Page(browser).post("token", {"label": "fast"})).json()["token"]
    async with httpx2.AsyncClient(base_url=site.base, timeout=30) as http:
        prog = Page(http, {"Authorization": f"Bearer {token}"})
        # All at once, so a slow store (Firestore) cannot refill the bucket between them.
        answers = await asyncio.gather(*(prog.get("info") for _ in range(100)))
        codes = [r.status_code for r in answers]
        assert codes.count(200) >= 40 and 429 in codes and set(codes) <= {200, 429}, codes
        over = next(r for r in answers if r.status_code == 429)
        assert int(over.headers["retry-after"]) >= 1 and "requests a second" in over.json()["error"]


@pytest.mark.asyncio
@needs_table
async def test_one_program_token_an_account_regenerated_or_revoked(site):
    async with site.browser(USER_A) as browser, httpx2.AsyncClient(base_url=site.base, timeout=30) as http:
        p = Page(browser)
        first = (await p.post("token", {"label": "bot"})).json()
        # A second while one exists: refused, and the first is still the one.
        r = await p.post("token", {"label": "another"})
        assert r.status_code == 409 and "regenerate" in r.json()["error"]
        assert (await p.get("token")).json()["token"]["id"] == first["id"]
        # Regenerate: the old token is refused at once, the new one works, still one, same name.
        new = (await p.post("token/regenerate", {})).json()
        assert new["token"] != first["token"] and new["label"] == "bot"
        old_prog = Page(http, {"Authorization": f"Bearer {first['token']}"})
        new_prog = Page(http, {"Authorization": f"Bearer {new['token']}"})
        assert (await old_prog.get("state")).status_code == 401
        assert (await new_prog.get("state")).status_code == 200
        assert (await p.get("token")).json()["token"]["id"] == new["id"]
        # Revoke, then make: works.
        assert (await p.post("token/revoke", {})).status_code == 200
        assert (await new_prog.get("state")).status_code == 401
        assert (await p.post("token/revoke", {})).status_code == 404
        again = await p.post("token", {"label": "bot 2"})
        assert again.status_code == 200
        assert (await Page(http, {"Authorization": f"Bearer {again.json()['token']}"}).get("state")).status_code == 200
        # Regenerate with none: makes one.
        assert (await p.post("token/revoke", {})).status_code == 200
        assert (await p.post("token/regenerate", {"label": "fresh"})).json()["label"] == "fresh"


@pytest.mark.asyncio
@needs_table
async def test_a_page_behind_a_finished_hand_is_shown_its_end(site):
    async with site.browser(USER_A) as browser, ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
        p = Page(browser)
        s = await _agent_decision(mcp)
        hand = s["hand"]
        while not s["over"]:
            s = _payload(await mcp.call_tool("act", {"action": _move(s)["kind"]}))
        final = s
        final_events = (await p.state())["finished"]["events"]
        s = _payload(await mcp.call_tool("next_hand", {}))
        # The page last saw `hand` unfinished: it gets that hand's end first ...
        w = await p.state(brief="1", hand=hand, over="0")
        assert w["catchUp"] and w["hand"] == hand and w["over"] and w["net"] == final["net"]
        # ... then, having shown it, the hand in play, with the history to fill in.
        w = await p.state(brief="1", hand=hand, over="1")
        assert "catchUp" not in w and w["hand"] == s["hand"] and hand in [h["number"] for h in w["history"]]
        assert w["presence"]["hands"][str(hand)] == ["your agent"]
        # Nothing new: no history resent.
        assert "history" not in await p.state(brief="1", hand=s["hand"])
        # A page replaying: the hands after the one it has, each with its moves.
        r = await p.state(since=str(hand - 1))
        assert r["history"][0]["number"] == hand and r["history"][0]["events"] == final_events
        assert (await p.get("state", since="x")).status_code == 400


@pytest.mark.asyncio
async def test_the_table_needs_sign_in_but_its_files_do_not(site):
    async with httpx2.AsyncClient(base_url=site.base, timeout=10) as http:
        for path in ("/table", "/table/"):
            r = await http.get(path)
            assert r.status_code == 303, (path, r.status_code)
            assert r.headers["location"].startswith("/table/signin?next="), r.headers["location"]
        # An old watch link (from before one table per login) goes to the table, whatever it says.
        r = await http.get("/table/watch/sometoken")
        assert r.status_code == 303 and r.headers["location"] == "/table/"
        r = await http.get("/table/api/state", params={"session": "x"})
        assert r.status_code == 401 and r.json()["signIn"].startswith("/table/signin")
        for method, path in (("POST", "/table/api/new"), ("POST", "/table/api/act"), ("POST", "/table/api/next"),
                             ("GET", "/table/api/token"), ("POST", "/table/api/token"), ("POST", "/table/api/token/regenerate"),
                             ("POST", "/table/api/token/revoke")):
            r = await http.request(method, path, json={} if method == "POST" else None)
            assert r.status_code == 401, (path, r.status_code)
        # The sign-in refuses to send anyone off-site afterwards.
        r = await http.get("/table/signin", params={"next": "https://evil.example/"})
        assert r.status_code == 303
        assert "evil" not in r.cookies.get("obgl_table_signin", "")


@pytest.mark.asyncio
async def test_a_hand_made_signin_cookie_is_refused_or_sent_to_the_table(site):
    """The sign-in cookie comes back from the browser: a malformed one is a
    clean 400, and one without `next` finishes the sign-in at /table/."""
    def cookie(obj):
        return base64.urlsafe_b64encode(json.dumps(obj).encode()).decode()

    async with httpx2.AsyncClient(base_url=site.base, timeout=10) as http:
        for bad in ("", "not base64!", cookie(["a list"]), cookie({"state": "s"}), cookie({"verifier": "v", "next": "/table/"}),
                    cookie({"state": 1, "verifier": "v"})):
            r = await http.get("/table/signin/callback", params={"state": "s", "code": "c"},
                               headers={"Cookie": f"obgl_table_signin={bad}"})
            assert r.status_code == 400, (bad, r.status_code)
        # A real sign-in whose cookie lost its `next`: back to the table, signed in.
        r = await http.get("/table/signin", params={"next": "/table/"})
        pending = json.loads(base64.urlsafe_b64decode(r.cookies["obgl_table_signin"]))
        r = await http.get(r.headers["location"])  # /authorize
        login = urlparse(r.headers["location"])
        back = await ot.click_sign_in(http, site.base, parse_qs(login.query)["request"][0])
        http.cookies.clear()
        del pending["next"]
        r = await http.get(back, headers={"Cookie": f"obgl_table_signin={cookie(pending)}"})
        assert r.status_code == 303 and r.headers["location"] == "/table/", (r.status_code, r.text)
        assert "obgl_table_at" in r.cookies


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
