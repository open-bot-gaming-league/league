"""Badges: what a coach's bot earns at its ranked tables (ranked.py), by
where it placed among the table's players, and the points they are worth.

    account-badges  account id -> {badges: [badge, ...]}, oldest first; a badge:
                    {badge, table, bot, version, season, points, place, players, awarded_at}

A finished table earns every badge whose place it reached: a win is also a
top 3 and a top 5 (default points 10 + 3 + 1). A badge names a place better
than the table's last (a top 5 needs six players or more), so a small table
earns only what it can mean. Each badge is awarded once per table: the
runner awards a table's badges before it marks the table done, so a runner
stopped in between plays the table again (the same seed: the same place)
and awards nothing new.

A badge keeps the points and the season it was awarded with: a coach's
points are summed per season (the season is a setting: OBGL_SEASON, shared
by the server and the runner), and their badges are kept for good.
"""

from __future__ import annotations

import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, replace

from oauth.store import AuthStore

SEASON = "2026-S1"


@dataclass(frozen=True)
class Rule:
    """A badge: earned at a table where the bot placed `place` or better."""

    id: str
    name: str
    place: int
    points: int


# The default points; OBGL_BADGE_POINTS sets them.
RULES = (
    Rule("win", "Gold", 1, 10),
    Rule("top-3", "Top 3", 3, 3),
    Rule("top-5", "Top 5", 5, 1),
)


@dataclass(frozen=True)
class BadgeSettings:
    season: str = SEASON
    rules: tuple[Rule, ...] = RULES


def earned(place: int, players: int, rules: tuple[Rule, ...] = RULES) -> list[Rule]:
    """The badges a place among `players` earns, best first."""
    return [r for r in rules if place <= r.place < players]


def settings_from_env(env: Mapping[str, str]) -> BadgeSettings:
    """OBGL_SEASON (default 2026-S1); OBGL_BADGE_POINTS: badge=points,...
    (e.g. win=10,top-3=3,top-5=1), any not named keeping its default."""
    s = BadgeSettings()
    if env.get("OBGL_SEASON"):
        s = replace(s, season=env["OBGL_SEASON"].strip())
    if env.get("OBGL_BADGE_POINTS"):
        points = {}
        for part in env["OBGL_BADGE_POINTS"].split(","):
            name, sep, value = part.strip().partition("=")
            if not sep or name not in {r.id for r in s.rules}:
                raise ValueError(f"OBGL_BADGE_POINTS: {part.strip()!r} is not badge=points for one of "
                                 f"{', '.join(r.id for r in s.rules)}")
            points[name] = int(value)
        s = replace(s, rules=tuple(replace(r, points=points.get(r.id, r.points)) for r in s.rules))
    return s


class Badges:
    """Each account's badges, in the auth store."""

    def __init__(self, store: AuthStore, settings: BadgeSettings = BadgeSettings(),
                 clock: Callable[[], float] = time.time) -> None:
        self.store, self.settings, self.clock = store, settings, clock
        self._names = {r.id: r.name for r in settings.rules}

    async def award(self, table_id: str, table: Mapping) -> list[dict]:
        """The badges a finished table earns its account (table: the
        ranked table, with its place and players), filed once: a table
        already awarded gets what it got then."""
        new = [{"badge": r.id, "table": table_id, "bot": table["bot"], "version": table["version"],
                "season": self.settings.season, "points": r.points, "place": table["place"],
                "players": table["players"], "awarded_at": int(self.clock())}
               for r in earned(table["place"], table["players"], self.settings.rules)]

        def change(rec):
            had = (rec or {"badges": []})["badges"]
            if not new or any(b["table"] == table_id for b in had):
                return None
            return {"badges": [*had, *new]}

        await self.store.update("account-badges", table["account"], change)
        return await self.for_table(table["account"], table_id)

    async def all(self, account: str) -> list[dict]:
        """The account's badges, oldest first."""
        return (await self.store.get("account-badges", account) or {"badges": []})["badges"]

    async def for_table(self, account: str, table_id: str) -> list[dict]:
        return [b for b in await self.all(account) if b["table"] == table_id]

    async def view(self, account: str) -> dict:
        """What GET /table/api/badges answers: this season's points, the
        points of every season, each kind of badge counted, the badges
        newest first, and the line the table page shows."""
        badges = await self.all(account)
        seasons: dict[str, int] = {}
        for b in badges:
            seasons[b["season"]] = seasons.get(b["season"], 0) + b["points"]
        season = self.settings.season
        counts = {r.id: sum(1 for b in badges if b["badge"] == r.id) for r in self.settings.rules}
        return {"season": season, "points": seasons.get(season, 0), "seasons": seasons, "counts": counts,
                "badges": [{**b, "name": self.name(b)} for b in _newest_table_first(badges)],
                "summary": self.summary(counts, seasons.get(season, 0))}

    def name(self, badge: Mapping) -> str:
        return self._names.get(badge["badge"], badge["badge"])

    def summary(self, counts: Mapping[str, int], points: int) -> str:
        """'Badges: 1 Gold, 2 Top 3, 4 Top 5. Season 2026-S1: 17 points.'"""
        have = [f"{n} {self._names[k]}" for k, n in counts.items() if n]
        kinds = ", ".join(have) if have else "none yet"
        return f"Badges: {kinds}. Season {self.settings.season}: {_points(points)}."

    def table_line(self, badges: list[dict], season_points: int) -> str:
        """What table_results ends with when a table earned badges."""
        each = ", ".join(f"{self.name(b)} ({_points(b['points'])})" for b in badges)
        return (f"Badges from this table: {each}. "
                f"Season {self.settings.season}: {_points(season_points)} in all.\n")


def _newest_table_first(badges: list[dict]) -> list[dict]:
    """Badges filed oldest first, a table's together and best first:
    the newest table's first, each table's still best first."""
    groups: list[list[dict]] = []
    for b in badges:
        if groups and groups[-1][0]["table"] == b["table"]:
            groups[-1].append(b)
        else:
            groups.append([b])
    return [b for g in reversed(groups) for b in g]


def _points(n: int) -> str:
    return f"{n} point" if n == 1 else f"{n} points"
