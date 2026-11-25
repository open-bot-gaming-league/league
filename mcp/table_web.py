"""The browser table at /table: the Go table's own page (cmd/table/static),
served by this server on the same origin as /mcp, with sign-in, Play and
Watch. Registered by server.py when OAuth is on (an owner needs an account).

    GET  /table/                     Play: the page (sign-in first if needed)
    GET  /table/watch/<watch token>  Watch: the same page, on a session the
                                     signed-in user owns (404 otherwise)
    GET  /table/<asset>              the page's files (table.js, table.css, cardmotion.js)
    GET  /table/signin?next=<path>   sign in (GitHub or Google), then back to <path>
    GET  /table/signin/callback      where /login sends the browser back
    POST /table/signout

    the page's API (JSON; every route but info needs a signed-in user: 401
    with {error, signIn} otherwise):
    GET  /table/api/info             the table's api/info + `league` (who is signed in)
    POST /table/api/new              {seat, opponent}: a new session, owned and played by you
    GET  /table/api/state            ?session=ID[&brief=1]: your session's state
    POST /table/api/act              {session, hand, step, kind[, amount]} or {..., action} (you must be in control)
    POST /table/api/next             {session, hand[, seat, opponent]} (you must be in control)
    GET  /table/api/watch            ?token=T[&hand=H&over=0|1][&brief=1]: read-only, no session id
    POST /table/api/takeover         {token}: Watch -> Play; the answer has the session id
    POST /table/api/tagin            {session}: Play -> your agent (it claims with claim_session())

Every state the page gets carries `control` (tables.py): who is playing,
the watch link, the switches so far, and whether this user can hand the
session to an agent. Another account's session and a session that does not exist answer 404 alike.

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
that keeps other sites from acting for a signed-in user.
"""

from __future__ import annotations

import base64
import hashlib
import html
import json
import secrets
from collections.abc import Callable
from typing import Any
from urllib.parse import urlencode

import anyio.to_thread
from mcp.server.mcpserver import MCPServer
from mcp.shared.auth import OAuthClientInformationFull, OAuthToken
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, RedirectResponse, Response

from oauth.provider import LeagueOAuthProvider, pkce_pair
from pages import render
from table_client import TableClient, TableError
from tables import AGENT, BROWSER, NotInControl, Party, TableSessions, controller_name

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
        tables: TableSessions,
        client: TableClient,
        public_url: str,
        resource_url: str,
    ) -> None:
        self.provider = provider
        self.tables = tables
        self.client = client
        self.public_url = public_url.rstrip("/")
        self.resource_url = resource_url
        self.redirect_uri = self.public_url + COOKIE_PATH + "/signin/callback"
        self.secure = self.public_url.startswith("https://")
        self._client_info: OAuthClientInformationFull | None = None

    # ---- links ---------------------------------------------------------------

    def watch_url(self, record: dict) -> str:
        return f"{self.public_url}{COOKIE_PATH}/watch/{record['watch']}"

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
        token = request.path_params.get("token")
        if token is not None and await self.tables.by_watch(token, account) is None:
            return self._finish(
                HTMLResponse(
                    render(
                        "No such session",
                        "<h1>No session here</h1><p>A watch link opens only for the account that started the "
                        "session, signed in the same way (GitHub or Google).</p>"
                        # The usual cause is being signed in as someone else: sign in again,
                        # and come back to this very link (not to a new session).
                        f'<p><a href="{html.escape(self.signin_url(request.url.path))}">Sign in as a different account</a></p>'
                        f'<p><a href="{COOKIE_PATH}/">Play at the table</a></p>',
                    ),
                    status_code=404,
                    headers=_PAGE_HEADERS,
                ),
                fresh,
            )
        return self._finish(Response(await self._page_html(), media_type="text/html; charset=utf-8", headers=_PAGE_HEADERS), fresh)

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

    async def _table(self, fn: Callable[[], dict]) -> dict:
        try:
            return await anyio.to_thread.run_sync(fn)
        except TableError as e:
            raise ApiError(e.status if e.status in (400, 404, 409) else 502, e.message) from e

    def control_view(self, record: dict) -> dict:
        c = record["controller"]
        return {
            "controller": c["kind"],
            "waiting": c["kind"] == AGENT and c.get("agent") is None,
            "yours": c["kind"] == BROWSER,
            "who": controller_name(c),
            "watch": f"watch/{record['watch']}",
            "switches": record["switches"],
        }

    def _view(self, state: dict, account: str, record: dict, *, watch: bool = False, history: bool = True) -> dict:
        out = dict(state)
        if watch:
            out.pop("session", None)
        if not history:
            out.pop("history", None)
        out["control"] = self.control_view(record)
        return out

    def api(self, handler: Callable, *, auth: bool = True) -> Callable:
        async def route(request: Request) -> Response:
            account, fresh = await self.signed_in(request)
            try:
                if auth and account is None:
                    raise ApiError(401, "sign in first", signIn=self.signin_url(COOKIE_PATH + "/"))
                body = await handler(request, account)
                response = JSONResponse(body, headers={"Cache-Control": "no-store"})
            except ApiError as e:
                response = JSONResponse(e.body, status_code=e.status, headers={"Cache-Control": "no-store"})
            return self._finish(response, fresh)

        return route

    async def _owned(self, session_id: Any, account: str) -> dict:
        record = await self.tables.table_session_for(session_id, account) if isinstance(session_id, str) else None
        if record is None:
            raise ApiError(404, "no such session")
        return record

    async def info(self, request: Request, account: str | None) -> dict:
        info = await self._table(self.client.info)
        who = None
        if account is not None:
            rec = await self.provider.account(account) or {}
            who = {"name": rec.get("display_name") or "you"}
        info["league"] = {"account": who, "signIn": self.signin_url(COOKIE_PATH + "/"), "signOut": COOKIE_PATH + "/signout"}
        return info

    async def new(self, request: Request, account: str) -> dict:
        body = await self._body(request)
        state = await self._table(lambda: self.client.new(seat=body.get("seat") or "alternate", opponent=body.get("opponent")))
        record = await self.tables.create(state["session"], account, Party.browser())
        self.tables.observe(state["session"], state)
        return self._view(state, account, record)

    async def state(self, request: Request, account: str) -> dict:
        session_id = request.query_params.get("session")
        record = await self._owned(session_id, account)
        state = await self._table(lambda: self.client.state(session_id))
        self.tables.observe(session_id, state)
        return self._view(state, account, record, history=request.query_params.get("brief") != "1")

    async def _require_browser(self, session_id: Any, account: str) -> dict:
        """The record, if this user's browser is in control (hold the lock)."""
        if not isinstance(session_id, str):
            raise ApiError(404, "no such session")
        try:
            return await self.tables.require_control(session_id, account, Party.browser())
        except LookupError:
            raise ApiError(404, "no such session") from None
        except NotInControl as e:
            raise ApiError(409, f"{controller_name(e.record['controller'])} is playing this session",
                           control=self.control_view(e.record)) from None

    async def _controlled(self, session_id: Any, account: str, step: Callable[[], dict]) -> dict:
        await self._owned(session_id, account)  # before the lock: no lock for a made-up id
        async with self.tables.lock(session_id):
            record = await self._require_browser(session_id, account)
            state = await self._table(step)
        self.tables.observe(session_id, state)
        return self._view(state, account, record)

    async def act(self, request: Request, account: str) -> dict:
        b = await self._body(request)
        # Either api/act form (cmd/table/API.md): a menu button's `action` index, or a
        # `kind` with an `amount` ("raise to" chips).
        return await self._controlled(
            b.get("session"), account,
            lambda: self.client.act(
                b["session"], b.get("hand"), b.get("step"),
                action=b.get("action"), kind=b.get("kind"), amount=b.get("amount"),
            ),
        )

    async def next(self, request: Request, account: str) -> dict:
        b = await self._body(request)
        return await self._controlled(
            b.get("session"), account,
            lambda: self.client.next(b["session"], b.get("hand"), seat=b.get("seat"), opponent=b.get("opponent")),
        )

    async def _watched(self, token: Any, account: str) -> tuple[str, dict]:
        found = await self.tables.by_watch(token, account) if isinstance(token, str) else None
        if found is None:
            raise ApiError(404, "no such session")
        return found

    async def watch(self, request: Request, account: str) -> dict:
        q = request.query_params
        session_id, record = await self._watched(q.get("token"), account)
        state = await self._table(lambda: self.client.state(session_id))
        self.tables.observe(session_id, state)
        try:
            shown = int(q["hand"])
        except (KeyError, ValueError):
            shown = None
        if shown is not None and shown < state["hand"] and q.get("over") != "1":
            # The page is still on a hand that has since ended: show it how,
            # from the final state this server saw, before the hand after it.
            final = self.tables.final_state(session_id, shown)
            if final is not None:
                return {**self._view(final, account, record, watch=True, history=False), "catchUp": True}
        # The history only when the page needs it: a first load, or a new hand
        # (it adds the hands it missed).
        history = q.get("brief") != "1" or shown != state["hand"]
        return self._view(state, account, record, watch=True, history=history)

    async def takeover(self, request: Request, account: str) -> dict:
        b = await self._body(request)
        session_id, _ = await self._watched(b.get("token"), account)
        async with self.tables.lock(session_id):
            state = await self._table(lambda: self.client.state(session_id))
            record = await self.tables.switch(session_id, account, Party.browser(), hand=state["hand"], step=state["step"])
        self.tables.observe(session_id, state)
        # From here the page plays: it gets the session id, as from api/new.
        return self._view(state, account, record)

    async def tagin(self, request: Request, account: str) -> dict:
        b = await self._body(request)
        session_id = b.get("session")
        await self._owned(session_id, account)
        async with self.tables.lock(session_id):
            await self._require_browser(session_id, account)
            # The browser hands its seat to the owner's agent: unclaimed
            # until the agent calls claim_session().
            state = await self._table(lambda: self.client.state(session_id))
            record = await self.tables.switch(
                session_id, account, Party(AGENT, None), hand=state["hand"], step=state["step"], by=Party.browser()
            )
        self.tables.observe(session_id, state)
        return self._view(state, account, record, watch=True)


def register_table_routes(server: MCPServer, web: TableWeb) -> None:
    # Both spellings answer directly (as /docs does): Starlette's slash
    # redirect would be built from the request scheme, http:// behind
    # Cloud Run's TLS termination.
    for path in (COOKIE_PATH, COOKIE_PATH + "/"):
        server.custom_route(path, methods=["GET"])(web.page)
    server.custom_route(COOKIE_PATH + "/watch/{token}", methods=["GET"])(web.page)
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
        ("POST", "new"): web.api(web.new),
        ("GET", "state"): web.api(web.state),
        ("POST", "act"): web.api(web.act),
        ("POST", "next"): web.api(web.next),
        ("GET", "watch"): web.api(web.watch),
        ("POST", "takeover"): web.api(web.takeover),
        ("POST", "tagin"): web.api(web.tagin),
    }
    for (method, name), route in api.items():
        server.custom_route(f"{COOKIE_PATH}/api/{name}", methods=[method])(route)
