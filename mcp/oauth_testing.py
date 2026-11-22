"""A scripted OAuth client for the connector, for tests and the local
proof (deploy/local/smoke.py). It does what claude.ai does --
dynamic client registration, /authorize with PKCE, the sign-in page,
/token -- with "Continue as guest" (or "Sign in with <IdP>", against
FakeIdP) clicked by a POST instead of a person; and what a browser does
to sign in at the table (/table/signin, table_sign_in). Not used by the
server and not copied into the image.
"""

from __future__ import annotations

import base64
import hashlib
import secrets
from contextlib import asynccontextmanager
from urllib.parse import parse_qs, urlencode, urljoin, urlparse

import httpx2
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, RedirectResponse
from starlette.routing import Route

REDIRECT_URI = "http://127.0.0.1:9/callback"  # never fetched: the code is read off the Location header


UPSTREAM_ID = "upstream-app-id"
UPSTREAM_SECRET = "upstream-app-secret"

class FakeIdP:
    """One fake upstream. `mode` decides what /authorize does: approve,
    deny (the user says no), or approve but fail at /token."""

    def __init__(self, userinfo: dict):
        self.userinfo = userinfo
        self.mode = "approve"
        self.codes: dict[str, dict] = {}
        self.tokens: dict[str, dict] = {}
        self.seen_authorize: list[dict] = []
        self.app = Starlette(
            routes=[
                Route("/authorize", self.authorize),
                Route("/token", self.token, methods=["POST"]),
                Route("/userinfo", self.user),
            ]
        )

    async def authorize(self, request: Request):
        q = dict(request.query_params)
        self.seen_authorize.append(q)
        assert q["client_id"] == UPSTREAM_ID
        assert q["code_challenge_method"] == "S256" and q["code_challenge"]
        if self.mode == "deny":
            return RedirectResponse(f"{q['redirect_uri']}?{urlencode({'error': 'access_denied', 'state': q['state']})}")
        code = secrets.token_urlsafe(8)
        self.codes[code] = q
        return RedirectResponse(f"{q['redirect_uri']}?{urlencode({'code': code, 'state': q['state']})}")

    async def token(self, request: Request):
        form = dict(await request.form())
        issued = self.codes.pop(form.get("code"), None)
        verifier = form.get("code_verifier", "")
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
        if (
            self.mode == "fail_token"
            or issued is None
            or form.get("client_id") != UPSTREAM_ID
            or form.get("client_secret") != UPSTREAM_SECRET
            or form.get("redirect_uri") != issued["redirect_uri"]
            or challenge != issued["code_challenge"]
        ):
            # GitHub's style: 200 with an error field.
            return JSONResponse({"error": "bad_verification_code"})
        access = secrets.token_urlsafe(8)
        self.tokens[access] = self.userinfo
        return JSONResponse({"access_token": access, "token_type": "bearer"})

    async def user(self, request: Request):
        auth = request.headers.get("authorization", "")
        info = self.tokens.get(auth.removeprefix("Bearer "))
        if info is None:
            return JSONResponse({"message": "Bad credentials"}, status_code=401)
        return JSONResponse(info)


def pkce_pair() -> tuple[str, str]:
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    return verifier, challenge


async def register(http: httpx2.AsyncClient, base: str, *, auth_method: str = "none", name: str = "test client") -> dict:
    r = await http.post(
        f"{base}/register",
        json={
            "redirect_uris": [REDIRECT_URI],
            "client_name": name,
            "token_endpoint_auth_method": auth_method,
            "grant_types": ["authorization_code", "refresh_token"],
            "response_types": ["code"],
        },
    )
    assert r.status_code == 201, (r.status_code, r.text)
    return r.json()


async def start_authorize(
    http: httpx2.AsyncClient, base: str, client: dict, *, challenge: str, state: str, resource: str | None
) -> str:
    """GET /authorize; returns the sign-in page's request id."""
    params = {
        "response_type": "code",
        "client_id": client["client_id"],
        "redirect_uri": REDIRECT_URI,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
        "state": state,
    }
    if resource:
        params["resource"] = resource
    r = await http.get(f"{base}/authorize?{urlencode(params)}")
    assert r.status_code == 302, (r.status_code, r.text)
    login = urlparse(r.headers["location"])
    assert login.path == "/login", r.headers["location"]
    return parse_qs(login.query)["request"][0]


def code_from_redirect(location: str, state: str) -> str:
    u = urlparse(location)
    assert f"{u.scheme}://{u.netloc}{u.path}" == REDIRECT_URI, location
    q = parse_qs(u.query)
    assert q.get("state") == [state], location
    assert "code" in q, location
    return q["code"][0]


async def guest_code(
    http: httpx2.AsyncClient, base: str, client: dict, *, resource: str | None = None
) -> tuple[str, str]:
    """The browser half, as a guest: /authorize, the sign-in page, the
    guest button. Returns (code, code_verifier)."""
    verifier, challenge = pkce_pair()
    state = secrets.token_urlsafe(8)
    request_id = await start_authorize(http, base, client, challenge=challenge, state=state, resource=resource)
    return code_from_redirect(await click_sign_in(http, base, request_id), state), verifier


def _client_auth(client: dict) -> dict:
    out = {"client_id": client["client_id"]}
    if client.get("client_secret"):
        out["client_secret"] = client["client_secret"]
    return out


async def token(http: httpx2.AsyncClient, base: str, client: dict, **form: str) -> httpx2.Response:
    return await http.post(f"{base}/token", data={**form, **_client_auth(client)})


async def exchange_code(
    http: httpx2.AsyncClient, base: str, client: dict, code: str, verifier: str, *, resource: str | None = None
) -> httpx2.Response:
    form = {"grant_type": "authorization_code", "code": code, "redirect_uri": REDIRECT_URI, "code_verifier": verifier}
    if resource:
        form["resource"] = resource
    return await token(http, base, client, **form)


async def refresh(http: httpx2.AsyncClient, base: str, client: dict, refresh_token: str) -> httpx2.Response:
    return await token(http, base, client, grant_type="refresh_token", refresh_token=refresh_token)


async def revoke(http: httpx2.AsyncClient, base: str, client: dict, tok: str) -> httpx2.Response:
    # mcp 2.3.0's RevocationRequest declares `client_secret: str | None`
    # with no default, so the form field is required even for a public
    # client: send it empty. (Its /token handler has no such quirk.)
    return await http.post(f"{base}/revoke", data={"token": tok, "client_secret": "", **_client_auth(client)})


async def guest_tokens(base: str, resource: str | None = None) -> tuple[dict, dict]:
    """The whole flow against a running server: (client, token response).
    `resource`: the token's resource (default `<base>/mcp`)."""
    resource = resource or f"{base}/mcp"
    async with httpx2.AsyncClient(timeout=10) as http:
        client = await register(http, base)
        code, verifier = await guest_code(http, base, client, resource=resource)
        r = await exchange_code(http, base, client, code, verifier, resource=resource)
        assert r.status_code == 200, r.text
        return client, r.json()


@asynccontextmanager
async def mcp_client(url: str, access_token: str):
    """An MCP Client that sends the bearer token on every request."""
    from mcp import Client
    from mcp.client.streamable_http import streamable_http_client

    async with httpx2.AsyncClient(headers={"Authorization": f"Bearer {access_token}"}, timeout=30) as http:
        async with Client(streamable_http_client(url, http_client=http)) as client:
            yield client


async def click_sign_in(http: httpx2.AsyncClient, base: str, request_id: str, button: str = "guest") -> str:
    """On the sign-in page for `request_id`, click `button` ("guest", or an
    IdP name: the fake IdP approves and sends the browser back). Returns
    where our server then sends the browser (the client's redirect URI)."""
    page = await http.get(f"{base}/login", params={"request": request_id})
    assert page.status_code == 200, page.text
    assert f'action="/login/{button}"' in page.text, page.text
    r = await http.post(f"{base}/login/{button}", data={"request": request_id})
    assert r.status_code == 303, (r.status_code, r.text)
    if button == "guest":
        return r.headers["location"]
    r = await http.get(r.headers["location"])  # the (fake) IdP signs the user in
    assert r.status_code in (302, 307), (r.status_code, r.text)
    r = await http.get(r.headers["location"])  # its redirect back to /login/<idp>/callback
    assert r.status_code == 303, (r.status_code, r.text)
    return r.headers["location"]


async def agent_tokens(base: str, button: str = "guest") -> dict:
    """A connector's token response, signed in with `button` (see click_sign_in)."""
    async with httpx2.AsyncClient(timeout=10) as http:
        client = await register(http, base)
        verifier, challenge = pkce_pair()
        state = secrets.token_urlsafe(8)
        request_id = await start_authorize(http, base, client, challenge=challenge, state=state, resource=f"{base}/mcp")
        code = code_from_redirect(await click_sign_in(http, base, request_id, button), state)
        r = await exchange_code(http, base, client, code, verifier, resource=f"{base}/mcp")
        assert r.status_code == 200, r.text
        return r.json()


async def table_sign_in(http: httpx2.AsyncClient, base: str, button: str = "guest", next_path: str = "/table/") -> str:
    """A browser signing in at the table: /table/signin -> /authorize -> the
    sign-in page -> `button` -> /table/signin/callback, which sets the
    table's cookies in `http`'s jar. Returns where the callback sends it."""
    r = await http.get(f"{base}/table/signin", params={"next": next_path})
    assert r.status_code == 303, (r.status_code, r.text)
    r = await http.get(urljoin(base, r.headers["location"]))  # /authorize
    assert r.status_code == 302, (r.status_code, r.text)
    login = urlparse(r.headers["location"])
    assert login.path == "/login", r.headers["location"]
    back = await click_sign_in(http, base, parse_qs(login.query)["request"][0], button)
    assert back.startswith(f"{base}/table/signin/callback?"), back
    r = await http.get(back)
    assert r.status_code == 303, (r.status_code, r.text)
    return r.headers["location"]
