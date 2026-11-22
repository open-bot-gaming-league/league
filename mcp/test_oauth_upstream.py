"""Sign-in through an upstream IdP (GitHub, Google), against a FAKE IdP:
a local Starlette app that speaks the authorization-code + PKCE flow the
real ones do, with their userinfo shapes. Our server runs in-process,
wired to it through the same github()/google() factories production
uses (only the endpoint URLs differ), both on real local sockets.

    python -m pytest mcp/test_oauth_upstream.py -q
"""

import base64
import hashlib
import json
import os
import secrets
import sys
from urllib.parse import parse_qs, urlencode, urlparse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2
import pytest
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, RedirectResponse
from starlette.routing import Route

import oauth_testing as ot
import server as server_module
from oauth import OAuthConfig
from oauth.idp import github, google
from oauth.store import MemoryStore

UPSTREAM_ID = "upstream-app-id"
UPSTREAM_SECRET = "upstream-app-secret"

USERS = {
    "github": ({"id": 4242, "login": "octo", "name": "Real Name Not Kept"}, "github:4242", "octo"),
    "google": ({"sub": "1098765", "name": "Ada L"}, "google:1098765", "Ada L"),
}


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


@pytest.fixture(params=["github", "google"])
def upstream(request, serve_asgi, free_port):
    """(idp name, fake IdP, our base URL, our store), both servers up."""
    name = request.param
    userinfo, _, _ = USERS[name]
    fake = FakeIdP(userinfo)
    with serve_asgi(fake.app) as idp_base:
        base = f"http://127.0.0.1:{free_port}"
        factory = {"github": github, "google": google}[name]
        idp = factory(
            UPSTREAM_ID,
            UPSTREAM_SECRET,
            f"{base}/login/{name}/callback",
            authorize_endpoint=f"{idp_base}/authorize",
            token_endpoint=f"{idp_base}/token",
            userinfo_endpoint=f"{idp_base}/userinfo",
        )
        store = MemoryStore()
        config = OAuthConfig(public_url=base, mcp_path="/mcp", store=store, idps=[idp])
        app = server_module.build_server("http://127.0.0.1:1/", config).streamable_http_app()
        with serve_asgi(app, free_port):
            yield name, fake, base, config


async def _through_upstream(http, base, name):
    """Register, /authorize, click "Sign in with <IdP>", let the fake IdP
    answer, follow its redirect to our callback. Returns (client,
    verifier, state, our callback's redirect Location)."""
    client = await ot.register(http, base, name="Claude")
    verifier, challenge = ot.pkce_pair()
    state = secrets.token_urlsafe(8)
    request_id = await ot.start_authorize(http, base, client, challenge=challenge, state=state, resource=f"{base}/mcp")
    page = await http.get(f"{base}/login", params={"request": request_id})
    assert f'action="/login/{name}"' in page.text
    r = await http.post(f"{base}/login/{name}", data={"request": request_id})
    assert r.status_code == 303
    to_idp = r.headers["location"]
    r = await http.get(to_idp)  # the fake IdP "signs the user in"
    assert r.status_code in (302, 307)
    back = r.headers["location"]
    assert back.startswith(f"{base}/login/{name}/callback?")
    r = await http.get(back)
    assert r.status_code == 303
    return client, verifier, state, r.headers["location"], to_idp


@pytest.mark.asyncio
async def test_sign_in_with_upstream_idp(upstream):
    name, fake, base, config = upstream
    _, subject, display = USERS[name]
    async with httpx2.AsyncClient(timeout=10) as http:
        client, verifier, state, location, to_idp = await _through_upstream(http, base, name)
        code = ot.code_from_redirect(location, state)
        r = await ot.exchange_code(http, base, client, code, verifier, resource=f"{base}/mcp")
        assert r.status_code == 200, r.text
        tok = r.json()

    # The upstream saw only our app and our callback -- never the MCP client.
    seen = fake.seen_authorize[-1]
    assert seen["client_id"] == UPSTREAM_ID and seen["client_id"] != client["client_id"]
    assert seen["redirect_uri"] == f"{base}/login/{name}/callback"
    assert ot.REDIRECT_URI not in to_idp
    assert seen.get("scope", "") == {"github": "", "google": "openid profile"}[name]

    access = await config.provider().load_access_token(tok["access_token"])
    assert access.subject == subject
    account = await config.provider().account(subject)
    assert account == {"provider": name, "provider_user_id": subject.split(":", 1)[1], "display_name": display}
    # Nothing else from the upstream profile is kept.
    assert "Real Name Not Kept" not in json.dumps(config.store._data)

    # And the token plays: the MCP endpoint takes it, tools answer.
    async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
        r = await mcp.call_tool("state", {})
        assert not r.is_error
        assert "new_session" in r.content[0].text  # "no active session: call new_session() first"


@pytest.mark.asyncio
async def test_upstream_denial_reaches_the_client_as_access_denied(upstream):
    name, fake, base, _ = upstream
    fake.mode = "deny"
    async with httpx2.AsyncClient(timeout=10) as http:
        _, _, state, location, _ = await _through_upstream(http, base, name)
    q = parse_qs(urlparse(location).query)
    assert q["error"] == ["access_denied"] and q["state"] == [state] and "code" not in q


@pytest.mark.asyncio
async def test_upstream_token_failure_reaches_the_client_as_server_error(upstream):
    name, fake, base, _ = upstream
    fake.mode = "fail_token"
    async with httpx2.AsyncClient(timeout=10) as http:
        _, _, state, location, _ = await _through_upstream(http, base, name)
    q = parse_qs(urlparse(location).query)
    assert q["error"] == ["server_error"] and q["state"] == [state] and "code" not in q


@pytest.mark.asyncio
async def test_callback_needs_a_live_request_started_with_that_idp(upstream):
    name, _, base, _ = upstream
    async with httpx2.AsyncClient(timeout=10) as http:
        r = await http.get(f"{base}/login/{name}/callback", params={"state": "forged", "code": "x"})
        assert r.status_code == 400
        # A real pending request, but the user never chose this IdP: refused.
        client = await ot.register(http, base)
        _, challenge = ot.pkce_pair()
        request_id = await ot.start_authorize(http, base, client, challenge=challenge, state="s", resource=None)
        r = await http.get(f"{base}/login/{name}/callback", params={"state": request_id, "code": "x"})
        assert r.status_code == 400
