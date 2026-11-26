"""The browser table at /table: the Go table's own page (cmd/table/static),
served by this server on the same origin as /mcp, with sign-in. Registered
by server.py when OAuth is on (an owner needs an account). Each account
has one table (tables.py), played by whoever moves first: the user here,
their agent over MCP, or their program with a program token (programs.py).

    GET  /table/                     the page (sign-in first if needed)
    GET  /table/watch/<anything>     an old watch link: sent to /table/
    GET  /table/<asset>              the page's files (table.js, table.css, cardmotion.js)
    GET  /table/signin?next=<path>   sign in (GitHub or Google), then back to <path>
    GET  /table/signin/callback      where /login sends the browser back
    POST /table/signout

    the table's API (JSON; every route but info needs the signed-in user's
    cookies or a program token, `Authorization: Bearer obgl_pt_...`: 401
    otherwise, with `signIn` for a browser). `session` is optional: the
    account's table is meant either way, and any other id is 404.
    GET  /table/api/info             the table's api/info + `league` (who is signed in)
    GET  /table/api/state            [?session=ID][&brief=1][&hand=H&over=0|1 | &since=H]: your table's state;
                                     since: with the finished hands after H, events and all (the latest 200)
    POST /table/api/act              {hand, step, kind[, amount]} or {hand, step, action}
    POST /table/api/next             {hand[, seat, opponent]}: seat and opponent from the page only (403 otherwise)
    POST /table/api/new              {seat, opponent}: reset: a new table, the score from zero (page only)
    GET  /table/api/tokens           your program tokens, without the tokens (page only)
    POST /table/api/tokens           {label}: a new program token, shown this once (page only)
    POST /table/api/tokens/revoke    {id}: revoke one (page only)

Every state carries `presence` (tables.py): the last move (who, how long
ago), the agent and programs seen lately, and who moved in each recent
hand. A program over its rate (programs.py) gets 429 with Retry-After.

Sign-in. The table is an OAuth client of this same server ("Bot Poker
table", registered here at start, public, PKCE): /table/signin sends the
browser through the SDK's /authorize to the sign-in page every connector
user sees (oauth/web.py), and the callback redeems the code in-process.
The tokens go in two HttpOnly, SameSite=Lax cookies scoped to /table:
the access token (1 h) and the refresh token (30 days, rotated through the
provider when the access token runs out). So a browser and an agent that
sign in with the same GitHub or Google account are the same owner.

Requests that change anything are POSTs with a JSON body, and refused if
the browser says they come from another origin: with SameSite=Lax cookies
that keeps other sites from acting for a signed-in user. A program's
token is sent by the program itself, never by a browser on its own, so it
needs no such check; a request carrying one is judged by it alone.
"""

from __future__ import annotations

import base64
import hashlib
import html
import json
import math
import secrets
from collections.abc import Callable
from typing import Any
from urllib.parse import urlencode

import anyio.to_thread
from mcp.server.mcpserver import MCPServer
from mcp.shared.auth import OAuthClientInformationFull, OAuthToken
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, RedirectResponse, Response

import programs
from oauth.provider import LeagueOAuthProvider, pkce_pair
from pages import render
from programs import Program, ProgramTokens
from table_client import TableClient, TableError
from tables import BROWSER, PROGRAM, Party, Tables

TABLE_CLIENT_ID = "obgl-table"
TABLE_CLIENT_NAME = "Bot Poker table"
ACCESS_COOKIE = "obgl_table_at"
REFRESH_COOKIE = "obgl_table_rt"
SIGNIN_COOKIE = "obgl_table_signin"
COOKIE_PATH = "/table"
ASSETS = ("table.js", "table.css", "cardmotion.js")
SIGNIN_TTL = 10 * 60
REFRESH_MAX_AGE = 30 * 24 * 60 * 60

_PAGE_HEADERS = {
    "Cache-Control": "no-store",
    # A watch link carries its token in the path: never hand it on.
    "Referrer-Policy": "no-referrer",
    "X-Frame-Options": "DENY",
    "Content-Security-Policy": (
        "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "
        "frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
    ),
}


class ApiError(Exception):
    def __init__(self, status: int, message: str, **extra: Any):
        super().__init__(message)
        self.status = status
        self.body = {"error": message, **extra}


def _safe_next(path: str | None) -> str:
    """Only a path on the table: no open redirect."""
    if path and path.startswith(COOKIE_PATH + "/") and not path.startswith("//") and "\\" not in path:
        return path
    return COOKIE_PATH + "/"


def _b64(obj: dict) -> str:
    return base64.urlsafe_b64encode(json.dumps(obj).encode()).decode()


def _unb64(value: str) -> dict | None:
    try:
        return json.loads(base64.urlsafe_b64decode(value.encode()))
    except (ValueError, json.JSONDecodeError):
        return None


def _pending_signin(value: str) -> dict | None:
    """The sign-in cookie as {state, verifier, next}, or None unless it has
    a string state and verifier; `next` is made safe (default: the table)."""
    pending = _unb64(value)
    if not isinstance(pending, dict) or not all(isinstance(pending.get(k), str) and pending[k] for k in ("state", "verifier")):
        return None
    next_path = pending.get("next")
    return {"state": pending["state"], "verifier": pending["verifier"],
            "next": _safe_next(next_path if isinstance(next_path, str) else None)}


class TableWeb:
    def __init__(
        self,
        provider: LeagueOAuthProvider,
        tables: Tables,
        programs: ProgramTokens,
        client: TableClient,
        public_url: str,
        resource_url: str,
    ) -> None:
        self.provider = provider
        self.tables = tables
        self.programs = programs
        self.client = client
        self.public_url = public_url.rstrip("/")
        self.resource_url = resource_url
        self.redirect_uri = self.public_url + COOKIE_PATH + "/signin/callback"
        self.secure = self.public_url.startswith("https://")
        self._client_info: OAuthClientInformationFull | None = None

    # ---- sign-in ----------------------------------------------------------------

    async def oauth_client(self) -> OAuthClientInformationFull:
        """The table's own OAuth client, registered with this server once
        per process (a put: the same record every time)."""
        if self._client_info is None:
            info = OAuthClientInformationFull(
                client_id=TABLE_CLIENT_ID,
                client_name=TABLE_CLIENT_NAME,
                redirect_uris=[self.redirect_uri],
                token_endpoint_auth_method="none",
                grant_types=["authorization_code", "refresh_token"],
                response_types=["code"],
            )
            await self.provider.register_client(info)
            self._client_info = info
        return self._client_info

    def _set_tokens(self, response: Response, tok: OAuthToken) -> None:
        common = {"path": COOKIE_PATH, "httponly": True, "samesite": "lax", "secure": self.secure}
        response.set_cookie(ACCESS_COOKIE, tok.access_token, max_age=tok.expires_in, **common)
        if tok.refresh_token:
            response.set_cookie(REFRESH_COOKIE, tok.refresh_token, max_age=REFRESH_MAX_AGE, **common)

    def _clear_tokens(self, response: Response) -> None:
        for name in (ACCESS_COOKIE, REFRESH_COOKIE):
            response.delete_cookie(name, path=COOKIE_PATH, secure=self.secure, httponly=True, samesite="lax")

    async def signed_in(self, request: Request) -> tuple[str | None, OAuthToken | None]:
        """(account, fresh tokens to set or None). The access cookie if it is
        still good, else one refresh through the provider (both rotate)."""
        access = request.cookies.get(ACCESS_COOKIE)
        if access:
            tok = await self.provider.load_access_token(access)
            if tok is not None and tok.client_id == TABLE_CLIENT_ID and tok.subject:
                return tok.subject, None
        refresh = request.cookies.get(REFRESH_COOKIE)
        if refresh:
            client = await self.oauth_client()
            rt = await self.provider.load_refresh_token(client, refresh)
            if rt is not None and rt.subject and (rt.expires_at is None or rt.expires_at >= self.provider.clock()):
                try:
                    return rt.subject, await self.provider.exchange_refresh_token(client, rt, rt.scopes)
                except Exception:  # noqa: BLE001  (a concurrent refresh used it first: signed out)
                    return None, None
        return None, None

    def signin_url(self, next_path: str) -> str:
        return f"{COOKIE_PATH}/signin?{urlencode({'next': next_path})}"

    async def signin(self, request: Request) -> Response:
        await self.oauth_client()
        verifier, challenge = pkce_pair()
        state = secrets.token_urlsafe(16)
        next_path = _safe_next(request.query_params.get("next"))
        params = {
            "response_type": "code",
            "client_id": TABLE_CLIENT_ID,
            "redirect_uri": self.redirect_uri,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
            "state": state,
            "resource": self.resource_url,
        }
        response = RedirectResponse(f"/authorize?{urlencode(params)}", status_code=303, headers={"Cache-Control": "no-store"})
        response.set_cookie(
            SIGNIN_COOKIE,
            _b64({"state": state, "verifier": verifier, "next": next_path}),
            max_age=SIGNIN_TTL,
            path=COOKIE_PATH + "/signin",
            httponly=True,
            samesite="lax",
            secure=self.secure,
        )
        return response

    async def callback(self, request: Request) -> Response:
        pending = _pending_signin(request.cookies.get(SIGNIN_COOKIE, ""))
        q = request.query_params
        if not pending or not q.get("state") or q.get("state") != pending.get("state"):
            return self._signin_failed("This sign-in was not started here, or it took too long.")
        if q.get("error") or not q.get("code"):
            return self._signin_failed("Sign-in was cancelled.", next_path=pending["next"])
        client = await self.oauth_client()
        code = await self.provider.load_authorization_code(client, q["code"])
        # The checks the SDK's /token makes, in-process: this client's code,
        # unexpired, for this redirect URI, and the PKCE verifier we hold.
        challenge = base64.urlsafe_b64encode(hashlib.sha256(pending["verifier"].encode()).digest()).decode().rstrip("=")
        if (
            code is None
            or code.expires_at < self.provider.clock()
            or str(code.redirect_uri) != self.redirect_uri
            or code.code_challenge != challenge
        ):
            return self._signin_failed("That sign-in link has expired or was already used.", next_path=pending["next"])
        try:
            tok = await self.provider.exchange_authorization_code(client, code)
        except Exception:  # noqa: BLE001  (TokenError: redeemed twice)
            return self._signin_failed("That sign-in link was already used.", next_path=pending["next"])
        response = RedirectResponse(pending["next"], status_code=303, headers={"Cache-Control": "no-store"})
        self._set_tokens(response, tok)
        response.delete_cookie(SIGNIN_COOKIE, path=COOKIE_PATH + "/signin")
        return response

    def _signin_failed(self, why: str, next_path: str = COOKIE_PATH + "/") -> Response:
        again = html.escape(self.signin_url(_safe_next(next_path)))
        return HTMLResponse(
            render("Sign-in failed", f"<h1>Not signed in</h1><p>{html.escape(why)}</p><p><a href=\"{again}\">Sign in again</a></p>"),
            status_code=400,
            headers=_PAGE_HEADERS,
        )

    async def signout(self, request: Request) -> Response:
        self._check_origin(request)
        access = request.cookies.get(ACCESS_COOKIE)
        if access:
            tok = await self.provider.load_access_token(access)
            if tok is not None and tok.client_id == TABLE_CLIENT_ID:
                await self.provider.revoke_token(tok)
        response = JSONResponse({"signedOut": True}, headers={"Cache-Control": "no-store"})
        self._clear_tokens(response)
        return response

    # ---- the page -------------------------------------------------------------

    async def _page_html(self) -> bytes:
        status, _, body = await anyio.to_thread.run_sync(lambda: self.client.asset(""))
        if status != 200:
            raise TableError(status, "the table did not serve its page")
        # The page's URLs are relative (API.md): one <base> makes them resolve
        # under /table/ from /table/watch/<token> too.
        head = b"<head>"
        assert body.count(head) == 1, "the table page has no single <head>"
        return body.replace(head, head + f'\n<base href="{COOKIE_PATH}/">'.encode(), 1)

    async def page(self, request: Request) -> Response:
        account, fresh = await self.signed_in(request)
        if account is None:
            return RedirectResponse(self.signin_url(request.url.path), status_code=303, headers={"Cache-Control": "no-store"})
        return self._finish(Response(await self._page_html(), media_type="text/html; charset=utf-8", headers=_PAGE_HEADERS), fresh)

    async def old_watch_link(self, request: Request) -> Response:  # noqa: ARG002
        """/table/watch/<token>, from before one table per login: the table
        itself (the owner's own, after sign-in), whatever the token."""
        return RedirectResponse(COOKIE_PATH + "/", status_code=303, headers={"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"})

    async def asset(self, name: str) -> Response:
        status, ctype, body = await anyio.to_thread.run_sync(lambda: self.client.asset(name))
        return Response(body, status_code=status, media_type=ctype, headers={"Cache-Control": "no-cache"})

    def _finish(self, response: Response, fresh: OAuthToken | None) -> Response:
        if fresh is not None:
            self._set_tokens(response, fresh)
        return response

    # ---- the page's API ---------------------------------------------------------

    def _check_origin(self, request: Request) -> None:
        origin = request.headers.get("origin")
        if origin is not None and origin.rstrip("/") != self.public_url:
            raise ApiError(403, "cross-origin request refused")

    async def _body(self, request: Request) -> dict:
        self._check_origin(request)
        if not request.headers.get("content-type", "").startswith("application/json"):
            raise ApiError(415, "send JSON")
        try:
            body = await request.json()
        except json.JSONDecodeError:
            raise ApiError(400, "bad JSON") from None
        if not isinstance(body, dict):
            raise ApiError(400, "send a JSON object")
        return body

    async def caller(self, request: Request) -> tuple[str | None, OAuthToken | None, Party | None]:
        """(account, fresh tokens to set or None, party). A request with an
        Authorization header is a program's: its program token, and nothing
        else, says who it is (cookies are not looked at). Otherwise the
        browser's sign-in cookies. (None, None, None): neither."""
        auth = request.headers.get("authorization")
        if auth is not None:
            scheme, _, token = auth.partition(" ")
            program = await self.programs.check(token.strip()) if scheme.lower() == "bearer" else None
            if program is None:
                raise ApiError(401, "that is not a program token for this table, or it was revoked: "
                                    "make one on the table page (Programs)")
            return program.account, None, Party.program(program.id, program.label)
        account, fresh = await self.signed_in(request)
        return account, fresh, Party.browser() if account else None

    def api(self, handler: Callable, *, auth: bool = True, browser_only: bool = False) -> Callable:
        async def route(request: Request) -> Response:
            fresh, headers = None, {"Cache-Control": "no-store"}
            try:
                account, fresh, party = await self.caller(request)
                if auth and account is None:
                    raise ApiError(401, "sign in first", signIn=self.signin_url(COOKIE_PATH + "/"))
                if browser_only and party is not None and party.kind != BROWSER:
                    raise ApiError(403, "only the table page can do this, signed in")
                if party is not None and party.kind == PROGRAM:
                    wait = self.programs.allow(Program(account, party.key, party.label))
                    if wait:
                        headers["Retry-After"] = str(max(1, math.ceil(wait)))
                        raise ApiError(429, f"slow down: at most {programs.RATE:g} requests a second per program token "
                                            "(for volume, play bot against bot with the SDK's local runner)")
                body = await handler(request, account, party)
                response = JSONResponse(body, headers=headers)
            except ApiError as e:
                response = JSONResponse(e.body, status_code=e.status, headers=headers)
            return self._finish(response, fresh)

        return route

    def _view(self, state: dict, session_id: str, *, history: bool = True) -> dict:
        out = dict(state)
        if not history:
            out.pop("history", None)
        out["presence"] = self.tables.presence(session_id)
        return out

    async def _table(self, account: str, party: Party, session_param: Any = None) -> tuple[str, dict]:
        """(session id, state) of the account's table, made if it has none.
        A session id other than the table's is 404, as for any session that
        is not this account's."""
        try:
            session_id, state = await self.tables.ensure(account)
        except TableError as e:
            raise ApiError(e.status if e.status in (400, 404, 409) else 502, e.message) from e
        if session_param not in (None, "") and session_param != session_id:
            raise ApiError(404, "no such session")
        self.tables.seen(session_id, party)
        self.tables.observe(session_id, state)
        return session_id, state

    async def _call(self, fn: Callable[[], Any]) -> Any:
        try:
            return await anyio.to_thread.run_sync(fn)
        except TableError as e:
            raise ApiError(e.status if e.status in (400, 404, 409) else 502, e.message) from e

    async def info(self, request: Request, account: str | None, party: Party | None) -> dict:
        info = await self._call(self.client.info)
        who = None
        if account is not None:
            rec = await self.provider.account(account) or {}
            who = {"name": rec.get("display_name") or "you"}
        info["league"] = {"account": who, "signIn": self.signin_url(COOKIE_PATH + "/"), "signOut": COOKIE_PATH + "/signout"}
        return info

    async def state(self, request: Request, account: str, party: Party) -> dict:
        q = request.query_params
        session_id, state = await self._table(account, party, q.get("session"))
        if "since" in q:
            # A page replaying hands: the finished hands after `since`, with
            # their events, as many as the table keeps (the latest 200).
            try:
                since = int(q["since"])
            except ValueError:
                raise ApiError(400, "since must be a hand number") from None
            state = await self._call(lambda: self.client.state(session_id, since=max(since, 0)))
            return self._view(state, session_id)
        try:
            shown = int(q["hand"])
        except (KeyError, ValueError):
            shown = None
        if shown is not None and shown < state["hand"] and q.get("over") != "1":
            # The page is still on a hand that has since ended: show it how,
            # from the final state this server saw, before the hand after it.
            final = self.tables.final_state(session_id, shown)
            if final is not None:
                return {**self._view(final, session_id, history=False), "catchUp": True}
        # The history only when asked for all of it, or on a new hand (the
        # page adds the hands it missed).
        history = q.get("brief") != "1" or (shown is not None and shown != state["hand"])
        return self._view(state, session_id, history=history)

    async def _move(self, account: str, party: Party, b: dict, step: Callable[[str, dict], dict], *, deal: bool) -> dict:
        """act / next on the account's table: whoever sends it first makes
        the pending decision; the Go table refuses (409) one meant for a
        decision already made. `deal`: the step deals the next hand."""
        session_id = await self._own_table(account, b.get("session"))
        async with self.tables.lock(session_id):
            state = await self._call(lambda: step(session_id, b))
            self.tables.moved(session_id, party, {"hand": b.get("hand"), "step": b.get("step"), "over": deal})
        self.tables.observe(session_id, state)
        return self._view(state, session_id)

    async def _own_table(self, account: str, session_param: Any) -> str:
        try:
            return await self.tables.resolve(account, session_param)
        except LookupError:
            raise ApiError(404, "no such session") from None

    def _act_step(self, session_id: str, b: dict) -> dict:
        # Either api/act form (cmd/table/API.md): a menu button's `action`
        # index, or a `kind` with an `amount` ("raise to" chips).
        return self.client.act(session_id, b.get("hand"), b.get("step"),
                               action=b.get("action"), kind=b.get("kind"), amount=b.get("amount"))

    def _next_step(self, session_id: str, b: dict) -> dict:
        return self.client.next(session_id, b.get("hand"), seat=b.get("seat"), opponent=b.get("opponent"))

    async def act(self, request: Request, account: str, party: Party) -> dict:
        return await self._move(account, party, await self._body(request), self._act_step, deal=False)

    async def next(self, request: Request, account: str, party: Party) -> dict:
        b = await self._body(request)
        if party.kind != BROWSER and (b.get("seat") is not None or b.get("opponent") is not None):
            raise ApiError(403, "settings are changed on the table page: the bot and the seat are the user's to pick there")
        return await self._move(account, party, b, self._next_step, deal=True)

    async def reset(self, request: Request, account: str, party: Party) -> dict:
        """api/new: a new table for this account (Reset score)."""
        b = await self._body(request)
        session_id, state = await self.tables.reset(account, seat=b.get("seat") or "alternate", opponent=b.get("opponent"))
        self.tables.moved(session_id, party, {"over": True})
        return self._view(state, session_id)

    async def tokens(self, request: Request, account: str, party: Party) -> dict:
        return {"tokens": await self.programs.list(account)}

    async def make_token(self, request: Request, account: str, party: Party) -> dict:
        b = await self._body(request)
        label = b.get("label")
        try:
            return await self.programs.create(account, label if isinstance(label, str) else "")
        except programs.TooMany as e:
            raise ApiError(409, str(e)) from None

    async def revoke_token(self, request: Request, account: str, party: Party) -> dict:
        b = await self._body(request)
        if not isinstance(b.get("id"), str) or not await self.programs.revoke(account, b["id"]):
            raise ApiError(404, "no such token")
        return {"revoked": b["id"]}


def register_table_routes(server: MCPServer, web: TableWeb) -> None:
    # Both spellings answer directly (as /docs does): Starlette's slash
    # redirect would be built from the request scheme, http:// behind
    # Cloud Run's TLS termination.
    for path in (COOKIE_PATH, COOKIE_PATH + "/"):
        server.custom_route(path, methods=["GET"])(web.page)
    server.custom_route(COOKIE_PATH + "/watch/{token}", methods=["GET"])(web.old_watch_link)
    server.custom_route(COOKIE_PATH + "/signin", methods=["GET"])(web.signin)
    server.custom_route(COOKIE_PATH + "/signin/callback", methods=["GET"])(web.callback)

    async def signout(request: Request) -> Response:
        try:
            return await web.signout(request)
        except ApiError as e:
            return JSONResponse(e.body, status_code=e.status)

    server.custom_route(COOKIE_PATH + "/signout", methods=["POST"])(signout)
    def _asset(name: str) -> None:
        @server.custom_route(f"{COOKIE_PATH}/{name}", methods=["GET"])
        async def serve(request: Request) -> Response:  # noqa: ARG001
            return await web.asset(name)

    for name in ASSETS:
        _asset(name)
    api = {
        ("GET", "info"): web.api(web.info, auth=False),
        ("GET", "state"): web.api(web.state),
        ("POST", "act"): web.api(web.act),
        ("POST", "next"): web.api(web.next),
        ("POST", "new"): web.api(web.reset, browser_only=True),
        ("GET", "tokens"): web.api(web.tokens, browser_only=True),
        ("POST", "tokens"): web.api(web.make_token, browser_only=True),
        ("POST", "tokens/revoke"): web.api(web.revoke_token, browser_only=True),
    }
    for (method, name), route in api.items():
        server.custom_route(f"{COOKIE_PATH}/api/{name}", methods=[method])(route)
