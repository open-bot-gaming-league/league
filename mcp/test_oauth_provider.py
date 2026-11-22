"""OAuth authorization server, in-process: the SDK's real routes
(metadata, /register, /authorize, /token, /revoke) around our provider,
driven over ASGI with no sockets; plus the store, the env config and the
provider's own rules (single-use codes, rotation, expiry, resource).

    python -m pytest mcp/test_oauth_provider.py -q
"""

import asyncio
import os
import stat
import sys
from urllib.parse import parse_qs, urlencode, urlparse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2
import pytest
import pytest_asyncio

import oauth_testing as ot
import server as server_module
from oauth import OAuthConfig
from oauth.idp import idps_from_env
from oauth.provider import PENDING_TTL, LeagueOAuthProvider, SignInError
from oauth.store import FileStore, MemoryStore, store_from_env

BASE = "http://127.0.0.1:8123"
RESOURCE = BASE + "/mcp"


class Clock:
    def __init__(self, t: float = 1_000_000_000.0):
        self.t = t

    def __call__(self) -> float:
        return self.t


@pytest.fixture
def config():
    return OAuthConfig(public_url=BASE, mcp_path="/mcp", store=MemoryStore())


@pytest_asyncio.fixture
async def http(config):
    app = server_module.build_server("http://127.0.0.1:1/", config).streamable_http_app()
    async with httpx2.AsyncClient(transport=httpx2.ASGITransport(app=app), base_url=BASE) as client:
        yield client


async def _signed_in(http, auth_method="none"):
    client = await ot.register(http, BASE, auth_method=auth_method)
    code, verifier = await ot.guest_code(http, BASE, client, resource=RESOURCE)
    return client, code, verifier


@pytest.mark.asyncio
async def test_metadata_points_at_our_endpoints(http):
    md = (await http.get("/.well-known/oauth-authorization-server")).json()
    assert md["issuer"] == BASE
    for key, path in [
        ("authorization_endpoint", "/authorize"),
        ("token_endpoint", "/token"),
        ("registration_endpoint", "/register"),
        ("revocation_endpoint", "/revoke"),
    ]:
        assert md[key] == BASE + path
    assert md["code_challenge_methods_supported"] == ["S256"]
    prm = (await http.get("/.well-known/oauth-protected-resource/mcp")).json()
    assert prm == {"resource": RESOURCE, "authorization_servers": [BASE], "bearer_methods_supported": ["header"]}


@pytest.mark.asyncio
@pytest.mark.parametrize("auth_method", ["none", "client_secret_post"])
async def test_register_authorize_token_refresh_revoke(http, config, auth_method):
    client, code, verifier = await _signed_in(http, auth_method)
    assert bool(client.get("client_secret")) == (auth_method != "none")
    provider = config.provider()

    r = await ot.exchange_code(http, BASE, client, code, verifier, resource=RESOURCE)
    assert r.status_code == 200, r.text
    tok = r.json()
    assert tok["token_type"] == "Bearer" and tok["expires_in"] == 3600
    access = await provider.load_access_token(tok["access_token"])
    assert access is not None
    assert access.client_id == client["client_id"]
    assert access.subject.startswith("guest:")
    assert access.resource == RESOURCE
    assert access.claims == {"iss": BASE}
    assert await provider.account(access.subject) == {
        "provider": "guest",
        "provider_user_id": access.subject.removeprefix("guest:"),
        "display_name": "Guest",
    }

    # Refresh rotates both tokens; the old pair stops working.
    r = await ot.refresh(http, BASE, client, tok["refresh_token"])
    assert r.status_code == 200, r.text
    tok2 = r.json()
    assert tok2["access_token"] != tok["access_token"] and tok2["refresh_token"] != tok["refresh_token"]
    assert await provider.load_access_token(tok["access_token"]) is None
    assert (await ot.refresh(http, BASE, client, tok["refresh_token"])).json()["error"] == "invalid_grant"
    access2 = await provider.load_access_token(tok2["access_token"])
    assert access2.subject == access.subject  # same account, so the same MCP sessions stay usable

    # Revoking the access token revokes its refresh token too.
    r = await ot.revoke(http, BASE, client, tok2["access_token"])
    assert r.status_code == 200
    assert await provider.load_access_token(tok2["access_token"]) is None
    assert (await ot.refresh(http, BASE, client, tok2["refresh_token"])).json()["error"] == "invalid_grant"


@pytest.mark.asyncio
async def test_revoking_the_refresh_token_revokes_the_access_token(http, config):
    client, code, verifier = await _signed_in(http)
    tok = (await ot.exchange_code(http, BASE, client, code, verifier)).json()
    assert (await ot.revoke(http, BASE, client, tok["refresh_token"])).status_code == 200
    assert await config.provider().load_access_token(tok["access_token"]) is None


@pytest.mark.asyncio
async def test_pkce_wrong_verifier_is_refused_and_code_stays_unspent(http):
    client, code, verifier = await _signed_in(http)
    wrong, _ = ot.pkce_pair()
    r = await ot.exchange_code(http, BASE, client, code, wrong)
    assert r.status_code == 400 and r.json()["error"] == "invalid_grant"
    assert "code_verifier" in r.json()["error_description"]
    # A failed PKCE check must not burn the code for its rightful holder.
    assert (await ot.exchange_code(http, BASE, client, code, verifier)).status_code == 200


@pytest.mark.asyncio
async def test_authorize_without_pkce_challenge_is_refused(http):
    client = await ot.register(http, BASE)
    params = {"response_type": "code", "client_id": client["client_id"], "redirect_uri": ot.REDIRECT_URI, "state": "s"}
    r = await http.get(f"/authorize?{urlencode(params)}")
    # Valid client + redirect_uri: the error goes back to the client (RFC 6749 4.1.2.1).
    assert r.status_code == 302
    q = parse_qs(urlparse(r.headers["location"]).query)
    assert q["error"] == ["invalid_request"] and q["state"] == ["s"]


@pytest.mark.asyncio
async def test_code_is_single_use(http):
    client, code, verifier = await _signed_in(http)
    assert (await ot.exchange_code(http, BASE, client, code, verifier)).status_code == 200
    r = await ot.exchange_code(http, BASE, client, code, verifier)
    assert r.status_code == 400 and r.json()["error"] == "invalid_grant"


@pytest.mark.asyncio
async def test_code_belongs_to_its_client(http):
    _, code, verifier = await _signed_in(http)
    other = await ot.register(http, BASE)
    r = await ot.exchange_code(http, BASE, other, code, verifier)
    assert r.status_code == 400 and r.json()["error"] == "invalid_grant"


@pytest.mark.asyncio
async def test_authorize_for_another_resource_is_refused(http):
    client = await ot.register(http, BASE)
    _, challenge = ot.pkce_pair()
    params = {
        "response_type": "code",
        "client_id": client["client_id"],
        "redirect_uri": ot.REDIRECT_URI,
        "code_challenge": challenge,
        "state": "s",
        "resource": "https://elsewhere.example/mcp",
    }
    r = await http.get(f"/authorize?{urlencode(params)}")
    assert r.status_code == 302
    assert parse_qs(urlparse(r.headers["location"]).query)["error"] == ["invalid_target"]


@pytest.mark.asyncio
async def test_sign_in_page(http):
    client = await ot.register(http, BASE, name="<script>x</script> Claude")
    _, challenge = ot.pkce_pair()
    request_id = await ot.start_authorize(http, BASE, client, challenge=challenge, state="s", resource=None)
    page = await http.get("/login", params={"request": request_id})
    assert page.status_code == 200
    assert "&lt;script&gt;x&lt;/script&gt; Claude" in page.text and "<script>" not in page.text
    assert "127.0.0.1:9" in page.text  # where the user will be sent back
    assert "Continue as guest" in page.text
    # No IdP configured in this fixture: no GitHub / Google buttons.
    assert "GitHub" not in page.text and "Google" not in page.text
    assert page.headers["x-frame-options"] == "DENY"
    assert "frame-ancestors 'none'" in page.headers["content-security-policy"]
    assert page.headers["cache-control"] == "no-store"
    assert page.headers["referrer-policy"] == "no-referrer"


@pytest.mark.asyncio
async def test_cancel_returns_access_denied_to_the_client(http):
    client = await ot.register(http, BASE)
    _, challenge = ot.pkce_pair()
    request_id = await ot.start_authorize(http, BASE, client, challenge=challenge, state="st", resource=None)
    r = await http.post("/login/cancel", data={"request": request_id})
    assert r.status_code == 303
    q = parse_qs(urlparse(r.headers["location"]).query)
    assert q["error"] == ["access_denied"] and q["state"] == ["st"]
    # The request is spent.
    assert (await http.post("/login/guest", data={"request": request_id})).status_code == 400


@pytest.mark.asyncio
async def test_unknown_or_reused_sign_in_request(http):
    assert (await http.get("/login", params={"request": "nope"})).status_code == 400
    assert (await http.post("/login/guest", data={"request": "nope"})).status_code == 400
    client = await ot.register(http, BASE)
    _, challenge = ot.pkce_pair()
    request_id = await ot.start_authorize(http, BASE, client, challenge=challenge, state="s", resource=None)
    assert (await http.post("/login/guest", data={"request": request_id})).status_code == 303
    assert (await http.post("/login/guest", data={"request": request_id})).status_code == 400
    # Unconfigured IdP: no route at all.
    assert (await http.post("/login/github", data={"request": request_id})).status_code in (404, 405)


@pytest.mark.asyncio
async def test_expiry_of_pending_and_access_token():
    clock = Clock()
    provider = LeagueOAuthProvider(MemoryStore(), issuer_url=BASE, resource_url=RESOURCE, clock=clock)
    from mcp.server.auth.provider import AuthorizationParams
    from mcp.shared.auth import OAuthClientInformationFull

    client = OAuthClientInformationFull(client_id="c", redirect_uris=[ot.REDIRECT_URI], token_endpoint_auth_method="none")
    await provider.register_client(client)
    params = AuthorizationParams(
        state="s", scopes=None, code_challenge="x", redirect_uri=ot.REDIRECT_URI, redirect_uri_provided_explicitly=True
    )
    url = await provider.authorize(client, params)
    request_id = parse_qs(urlparse(url).query)["request"][0]
    clock.t += PENDING_TTL + 1
    with pytest.raises(SignInError):
        await provider.finish_guest(request_id)

    url = await provider.authorize(client, params)
    redirect = await provider.finish_guest(parse_qs(urlparse(url).query)["request"][0])
    code = parse_qs(urlparse(redirect).query)["code"][0]
    tok = await provider.exchange_authorization_code(client, await provider.load_authorization_code(client, code))
    assert await provider.load_access_token(tok.access_token) is not None
    clock.t += 3601
    assert await provider.load_access_token(tok.access_token) is None


def test_table_session_owner_survives_restart_and_is_owner_only(tmp_path):
    path = str(tmp_path / "oauth.json")

    def provider():
        return LeagueOAuthProvider(FileStore(path), issuer_url=BASE, resource_url=RESOURCE)

    async def record():
        p = provider()
        await p.record_table_session("t-guest-a", "guest:aaa")
        await p.record_table_session("t-guest-b", "guest:bbb")
        await p.record_table_session("t-github", "github:4242")

    asyncio.run(record())

    async def read():
        p = provider()  # a fresh process's view: the file reopened
        assert (await p.table_session_for("t-guest-a", "guest:aaa"))["owner"] == "guest:aaa"
        assert (await p.table_session_for("t-github", "github:4242"))["owner"] == "github:4242"
        # Another account (another guest, or a signed-in user) reads nothing.
        assert await p.table_session_for("t-guest-a", "guest:bbb") is None
        assert await p.table_session_for("t-guest-a", "github:4242") is None
        assert await p.table_session_for("t-github", "guest:aaa") is None
        # A guest matches only its own exact id: no prefix or provider-only match.
        assert await p.table_session_for("t-guest-a", "guest:") is None
        assert await p.table_session_for("t-guest-a", "guest:aa") is None
        assert await p.table_session_for("nope", "guest:aaa") is None

    asyncio.run(read())


def test_file_store_survives_reopen_and_drops_expired(tmp_path):
    path = str(tmp_path / "sub" / "oauth.json")

    async def fill():
        s = FileStore(path)
        await s.put("clients", "c1", {"client_id": "c1"})
        await s.put("access", "live", {"expires_at": 4_000_000_000})
        await s.put("access", "dead", {"expires_at": 1})
        await s.put("codes", "once", {"x": 1})
        assert await s.take("codes", "once") == {"x": 1}
        assert await s.take("codes", "once") is None

    asyncio.run(fill())
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600
    s2 = FileStore(path)

    async def read():
        assert await s2.get("clients", "c1") == {"client_id": "c1"}
        assert await s2.get("access", "live") is not None
        assert await s2.get("access", "dead") is None
        assert await s2.get("codes", "once") is None

    asyncio.run(read())
    assert [p.name for p in (tmp_path / "sub").iterdir()] == ["oauth.json"]  # no temp files left


def test_store_from_env(tmp_path):
    assert isinstance(store_from_env({}), MemoryStore)
    s = store_from_env({"OBGL_AUTH_STORE": "file", "OBGL_STATE_DIR": str(tmp_path)})
    assert isinstance(s, FileStore) and s.path == str(tmp_path / "oauth.json")
    assert store_from_env({"OBGL_AUTH_STORE": "file", "OBGL_AUTH_FILE": str(tmp_path / "a.json")}).path.endswith("a.json")
    with pytest.raises(ValueError):
        store_from_env({"OBGL_AUTH_STORE": "file"})
    with pytest.raises(ValueError):
        store_from_env({"OBGL_AUTH_STORE": "s3"})


def test_idps_from_env():
    assert idps_from_env({}, BASE) == []
    idps = idps_from_env({"OBGL_GITHUB_CLIENT_ID": "i", "OBGL_GITHUB_CLIENT_SECRET": "s"}, BASE)
    assert [i.name for i in idps] == ["github"]
    assert idps[0].redirect_uri == BASE + "/login/github/callback"
    both = idps_from_env(
        {
            "OBGL_GITHUB_CLIENT_ID": "i",
            "OBGL_GITHUB_CLIENT_SECRET": "s",
            "OBGL_GOOGLE_CLIENT_ID": "g",
            "OBGL_GOOGLE_CLIENT_SECRET": "t",
            "OBGL_GOOGLE_REDIRECT_URI": "https://proxy.example/login/google/callback",
        },
        BASE,
    )
    assert [i.name for i in both] == ["github", "google"]
    assert both[1].redirect_uri == "https://proxy.example/login/google/callback"
    with pytest.raises(ValueError, match="both"):
        idps_from_env({"OBGL_GOOGLE_CLIENT_ID": "g"}, BASE)
    with pytest.raises(ValueError, match="path"):
        idps_from_env(
            {"OBGL_GITHUB_CLIENT_ID": "i", "OBGL_GITHUB_CLIENT_SECRET": "s", "OBGL_GITHUB_REDIRECT_URI": BASE + "/cb"},
            BASE,
        )


def test_oauth_config():
    assert OAuthConfig.from_env({}, "/mcp") is None
    c = OAuthConfig.from_env({"OBGL_PUBLIC_URL": "https://obgl.example/"}, "/mcp")
    assert c.public_url == "https://obgl.example" and c.resource_url == "https://obgl.example/mcp"
    for bad in ["obgl.example", "https://obgl.example/sub", "https://obgl.example?x=1"]:
        with pytest.raises(ValueError):
            OAuthConfig(public_url=bad, mcp_path="/mcp")
