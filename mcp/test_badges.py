"""Badges (badges.py): the rules (a place among the players earns every
badge it reached), once per table, points per season; the runner awards a
finished table's (a fake rankedtable here: no go needed); GET
/table/api/badges, the tables list and table_results show them; the table
page asks for them."""

import hashlib
import json
import os
import stat
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2
import pytest

import oauth_testing as ot
from badges import RULES, SEASON, Badges, BadgeSettings, earned, settings_from_env
from bot_files import bot_key
from conftest import USER_A, USER_B, Wiring
from oauth.store import MemoryStore
from ranked import QUEUE, RankedTables, Runner
from test_table_results import TEXT, table_url  # noqa: F401 (the fake Go table: one ranked table, table-p8)

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
STATIC = os.path.join(REPO, "poker", "engine", "cmd", "table", "static")
ME = "github:1001"  # USER_A's account


def _table(place, players=11, bot="mybot", version=1, account=ME):
    return {"account": account, "bot": bot, "version": version, "place": place, "players": players}


def test_places_earn_every_badge_they_reach():
    ids = lambda place, players=11: [r.id for r in earned(place, players)]  # noqa: E731
    assert ids(1) == ["win", "top-3", "top-5"]
    assert ids(3) == ["top-3", "top-5"]
    assert ids(5) == ["top-5"]
    assert ids(6) == [] and ids(11) == []
    # A badge needs a place better than the table's last: a top 5 of five means nothing.
    assert ids(4, players=5) == [] and ids(3, players=5) == ["top-3"]
    assert ids(1, players=4) == ["win", "top-3"] and ids(1, players=2) == ["win"]
    assert [(r.id, r.points) for r in RULES] == [("win", 10), ("top-3", 3), ("top-5", 1)]


@pytest.mark.asyncio
async def test_a_table_is_awarded_once():
    store = MemoryStore()
    badges = Badges(store, clock=lambda: 1000)
    got = await badges.award("t1", _table(3))
    assert [(b["badge"], b["points"], b["season"], b["table"], b["bot"], b["version"], b["place"], b["players"])
            for b in got] == [("top-3", 3, SEASON, "t1", "mybot", 1, 3, 11), ("top-5", 1, SEASON, "t1", "mybot", 1, 3, 11)]
    # Again (a runner that stopped before marking it done): nothing new, even at another place.
    assert await badges.award("t1", _table(3)) == got
    assert await badges.award("t1", _table(1)) == got
    assert await badges.award("t2", _table(6)) == []
    assert len(await badges.all(ME)) == 2
    assert (await badges.view(ME))["points"] == 4


@pytest.mark.asyncio
async def test_points_are_per_season_and_badges_are_kept():
    store = MemoryStore()
    one = Badges(store, BadgeSettings(season="S1"))
    await one.award("a", _table(1))  # 14
    await one.award("b", _table(5))  # 1
    two = Badges(store, BadgeSettings(season="S2"))
    await two.award("c", _table(2))  # 4
    view = await two.view(ME)
    assert view["season"] == "S2" and view["points"] == 4
    assert view["seasons"] == {"S1": 15, "S2": 4}
    assert view["counts"] == {"win": 1, "top-3": 2, "top-5": 3}
    assert [b["table"] for b in view["badges"]] == ["c", "c", "b", "a", "a", "a"]  # newest first, every season
    assert [b["name"] for b in view["badges"]] == ["Top 3", "Top 5", "Top 5", "Gold", "Top 3", "Top 5"]
    assert view["summary"] == "Badges: 1 Gold, 2 Top 3, 3 Top 5. Season S2: 4 points."
    assert (await two.view("github:2002"))["summary"] == "Badges: none yet. Season S2: 0 points."
    # A badge keeps the points it was awarded with.
    retuned = Badges(store, settings_from_env({"OBGL_SEASON": "S1", "OBGL_BADGE_POINTS": "win=100"}))
    assert (await retuned.view(ME))["points"] == 15
    await retuned.award("d", _table(1))
    assert (await retuned.view(ME))["points"] == 15 + 100 + 3 + 1


def test_settings_from_the_environment():
    assert settings_from_env({}) == BadgeSettings()
    s = settings_from_env({"OBGL_SEASON": "2027-S1", "OBGL_BADGE_POINTS": "win=20, top-5=2"})
    assert s.season == "2027-S1" and [(r.id, r.points) for r in s.rules] == [("win", 20), ("top-3", 3), ("top-5", 2)]
    for bad in ("gold=5", "win", "win=x"):
        with pytest.raises(ValueError):
            settings_from_env({"OBGL_BADGE_POINTS": bad})


FAKE_RANKEDTABLE = """#!{python}
import json, os, sys
out = sys.argv[sys.argv.index("-out") + 1]
place, players = int(os.environ["FAKE_PLACE"]), 11
others = [p for p in range(1, players + 1) if p != place]
json.dump({{"format": 1, "seats": [{{}}] * players,
           "places": [{{"seat": 0, "place": place}}] + [{{"seat": i + 1, "place": p}} for i, p in enumerate(others)]}},
          open(out, "w"))
"""


class Records:
    def __init__(self):
        self.put_ids = []

    def put(self, table_id, record):
        self.put_ids.append(table_id)


async def _queued(tables, store, files_dir, account=ME):
    """A finalized version on file (its module in the bot store) and its table queued."""
    wasm = b"\0asm-not-really"
    sha = hashlib.sha256(wasm).hexdigest()
    path = os.path.join(files_dir, bot_key(account, sha))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as f:
        f.write(wasm)
    await store.put("bot-submissions", "sub-1", {"account": account, "state": "finalized", "name": "mybot", "version": 1,
                                                 "wasm": {"sha256": sha, "size": len(wasm)}, "data": []})
    return await tables.enqueue(account, {"name": "mybot", "version": 1, "submission": "sub-1"})


def _runner(tables, tmp_path, place):
    command = tmp_path / "rankedtable"
    command.write_text(FAKE_RANKEDTABLE.format(python=sys.executable))
    command.chmod(command.stat().st_mode | stat.S_IXUSR)
    return Runner(tables, str(command), str(tmp_path), str(tmp_path / "files"), Records(),
                  env={**os.environ, "FAKE_PLACE": str(place)})


@pytest.mark.asyncio
async def test_the_runner_awards_a_finished_tables_badges(tmp_path):
    store = MemoryStore()
    tables = RankedTables(store, badges=BadgeSettings(season="S9"))
    table = await _queued(tables, store, str(tmp_path / "files"))
    runner = _runner(tables, tmp_path, 1)
    await runner.drain()
    listed = (await tables.list(ME))["tables"]
    assert [(t["id"], t["state"], t["place"], t["players"], t["badges"]) for t in listed] == \
        [(table["id"], "done", 1, 11, ["Gold", "Top 3", "Top 5"])]
    view = await tables.badges.view(ME)
    assert view["points"] == 14 and view["season"] == "S9"
    assert {b["table"] for b in view["badges"]} == {table["id"]}

    # Stopped after the award, before the table was done: the next runner
    # plays it again and awards nothing new.
    await store.put("ranked-tables", table["id"], {**await store.get("ranked-tables", table["id"]), "state": "queued"})
    await store.put("ranked-queue", QUEUE, {"tables": [table["id"]]})
    await runner.drain()
    assert (await store.get("ranked-tables", table["id"]))["state"] == "done"
    assert len(await tables.badges.all(ME)) == 3 and (await tables.badges.view(ME))["points"] == 14


@pytest.mark.asyncio
async def test_a_table_outside_the_top_five_earns_none(tmp_path):
    store = MemoryStore()
    tables = RankedTables(store)
    table = await _queued(tables, store, str(tmp_path / "files"))
    await _runner(tables, tmp_path, 6).drain()
    assert (await tables.list(ME))["tables"][0]["badges"] == []
    assert await store.get("account-badges", ME) is None


async def _token(site, user=USER_A):
    async with site.browser(user) as browser:
        return (await browser.post("/table/api/token", json={"label": "badges test"})).json()["token"]


@pytest.mark.asyncio
async def test_the_api_and_table_results_show_the_badges(bot_site, table_url):  # noqa: F811
    """The fake Go table keeps one ranked table, table-p8: here it is the
    signed-in account's newest, done, placed 2nd of 11."""
    made = {}

    def wiring(base, store):
        made["ranked"] = RankedTables(store, badges=BadgeSettings(season="S1"))
        made["store"] = store
        return Wiring(None, made["ranked"])

    with bot_site(wiring, table_url=table_url) as site:
        store, ranked = made["store"], made["ranked"]
        await store.put("ranked-tables", "table-p8", {"account": ME, "bot": "mybot", "version": 2, "submission": "s",
                                                      "seed": 1, "state": "done", "queued_at": 1, "attempts": 1,
                                                      "finished_at": 2, "place": 2, "players": 11})
        await store.put("account-ranked", ME, {"tables": ["table-p8"]})
        await ranked.badges.award("old", _table(5, version=1))
        await ranked.badges.award("table-p8", _table(2, version=2))

        auth = {"Authorization": f"Bearer {await _token(site)}"}
        async with httpx2.AsyncClient(base_url=site.base, timeout=30) as http:
            view = (await http.get("/table/api/badges", headers=auth)).json()
            assert view["season"] == "S1" and view["points"] == 1 + 3 + 1
            assert view["summary"] == "Badges: 1 Top 3, 2 Top 5. Season S1: 5 points."
            assert [(b["badge"], b["table"], b["url"]) for b in view["badges"]] == [
                ("top-3", "table-p8", "/table/results/table-p8"), ("top-5", "table-p8", "/table/results/table-p8"),
                ("top-5", "old", "/table/results/old")]
            tables = (await http.get("/table/api/tables", headers=auth)).json()["tables"]
            assert tables[0]["badges"] == ["Top 3", "Top 5"]
            other = {"Authorization": f"Bearer {await _token(site, USER_B)}"}
            assert (await http.get("/table/api/badges", headers=other)).json()["summary"] == \
                "Badges: none yet. Season S1: 0 points."
            assert (await http.get("/table/api/badges")).status_code == 401

        line = "\nBadges from this table: Top 3 (3 points), Top 5 (1 point). Season S1: 5 points in all.\n"
        async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_A)) as mcp:
            for args in ({}, {"table_id": "table-p8"}):
                text = (await mcp.call_tool("table_results", args)).content[0].text
                assert text.endswith(f"/table/results/table-p8\n{line}"), text
        # Another account reading the same table: the report, none of the owner's badges.
        async with ot.mcp_client(f"{site.base}/mcp", await site.agent_token(USER_B)) as mcp:
            text = (await mcp.call_tool("table_results", {"table_id": "table-p8"})).content[0].text
            assert text.startswith(TEXT) and "Badges" not in text


@pytest.mark.asyncio
async def test_without_ranked_tables_the_badges_route_says_so(site):
    async with site.browser(USER_A) as browser:
        r = await browser.get("/table/api/badges")
    assert r.status_code == 503 and "ranked tables are not set up" in r.json()["error"]


def test_the_table_page_shows_the_badges_line():
    with open(os.path.join(STATIC, "index.html"), encoding="utf-8") as f:
        html = f.read()
    with open(os.path.join(STATIC, "table.js"), encoding="utf-8") as f:
        js = f.read()
    ranked = html[html.index('<details id="ranked"'):html.index("</details>", html.index('<details id="ranked"'))]
    assert 'id="ranked-badges"' in ranked and "earns a badge" in ranked
    assert "call('GET', 'api/badges')" in js and "$('ranked-badges').textContent = badges.summary" in js
    assert "t.badges.join(', ')" in js
