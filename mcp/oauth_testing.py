"""A scripted OAuth client for the connector, for tests and the local
proof (deploy/local/oauth_smoke.py). It does what claude.ai does --
dynamic client registration, /authorize with PKCE, the sign-in page,
/token -- with "Continue as guest" clicked by a POST instead of a person.
Not used by the server and not copied into the image.
"""

from __future__ import annotations

import base64
import hashlib
import re
import secrets
from contextlib import asynccontextmanager
from urllib.parse import parse_qs, urlencode, urlparse

import httpx2

REDIRECT_URI = "http://127.0.0.1:9/callback"  # never fetched: the code is read off the Location header


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
    page = await http.get(f"{base}/login", params={"request": request_id})
    assert page.status_code == 200, page.text
    assert re.search(r'action="/login/guest"', page.text), page.text
    r = await http.post(f"{base}/login/guest", data={"request": request_id})
    assert r.status_code == 303, (r.status_code, r.text)
    return code_from_redirect(r.headers["location"], state), verifier


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


async def guest_tokens(base: str) -> tuple[dict, dict]:
    """The whole flow against a running server: (client, token response)."""
    async with httpx2.AsyncClient(timeout=10) as http:
        client = await register(http, base)
        code, verifier = await guest_code(http, base, client, resource=f"{base}/mcp")
        r = await exchange_code(http, base, client, code, verifier, resource=f"{base}/mcp")
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
