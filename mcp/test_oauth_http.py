"""OAuth over real HTTP: our server in-process on a socket with OAuth on
(so /mcp needs a bearer token), signed in through a fake GitHub, the MCP
tools called with the token that yields, and the store carrying tokens
across a server restart. Every test runs with the file store, and again
with Firestore when FIRESTORE_EMULATOR_HOST names an emulator
(test_oauth_store.py says how to start one).

The table-touching tests (table + act) skips itself if the live
table at localhost:9777/poker/ is not reachable; the rest need no table.

    python -m pytest mcp/test_oauth_http.py -q
"""

import json
import os
import sys
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2
import pytest

import oauth_testing as ot
from conftest import USER_A, USER_B, league_app
from oauth.provider import LeagueOAuthProvider
from oauth.store import FileStore, FirestoreStore
from table_client import TableClient
from tables import Tables


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


INIT = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}},
}
MCP_HEADERS = {"Accept": "application/json, text/event-stream", "Content-Type": "application/json"}


@pytest.fixture(params=["file", "firestore"])
def open_store(request, tmp_path):
    """A function opening the server's durable store (each call: a fresh
    handle on the same data, as a restarted server sees it)."""
    if request.param == "file":
        path = str(tmp_path / "oauth.json")
        return lambda: FileStore(path)
    if not os.environ.get("FIRESTORE_EMULATOR_HOST"):
        pytest.skip("FIRESTORE_EMULATOR_HOST not set (no Firestore emulator)")
    project = f"demo-obgl-{uuid.uuid4().hex[:12]}"  # a fresh, empty emulator project
    return lambda: FirestoreStore(project)


@pytest.fixture
def oauth_server(serve_asgi, free_port, open_store):
    """Our server with OAuth on and a durable store, GitHub sign-in against
    a fake IdP; yields (base, start, fake) where start() is a context manager
    running the server on the same port and store (call it again after the
    first exits: a restart) and fake.userinfo is who signs in next."""
    base = f"http://127.0.0.1:{free_port}"
    fake = ot.FakeIdP(USER_A)
    with serve_asgi(fake.app) as idp_base:
        yield base, (lambda: serve_asgi(league_app(base, idp_base, open_store()), free_port)), fake


def _payload(result):
    assert not result.is_error, result.content
    if result.structured_content is not None:
        return result.structured_content
    return json.loads(result.content[0].text)


@pytest.mark.asyncio
async def test_mcp_needs_a_token_and_says_where_to_get_one(oauth_server):
    base, start, _ = oauth_server
    with start():
        async with httpx2.AsyncClient() as http:
            r = await http.post(f"{base}/mcp", json=INIT, headers=MCP_HEADERS)
            assert r.status_code == 401
            assert f'resource_metadata="{base}/.well-known/oauth-protected-resource/mcp"' in r.headers["www-authenticate"]
            r = await http.post(f"{base}/mcp", json=INIT, headers={**MCP_HEADERS, "Authorization": "Bearer obgl_at_nope"})
            assert r.status_code == 401
            # The docs and the landing page stay public.
            assert (await http.get(f"{base}/docs/AGENTS.md")).status_code == 200
            landing = (await http.get(f"{base}/")).text
            assert "GitHub or Google" in landing and "guest" not in landing.lower()


@pytest.mark.asyncio
async def test_sign_in_then_tools(oauth_server):
    base, start, _ = oauth_server
    with start():
        _, tok = await ot.sign_in_tokens(base)
        async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
            assert "Bot Poker" in (await mcp.call_tool("docs", {"name": "rules"})).content[0].text


@pytest.mark.asyncio
@pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")
async def test_signed_in_agent_plays_a_hand(oauth_server):
    base, start, _ = oauth_server
    with start():
        _, tok = await ot.sign_in_tokens(base)
        async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
            state = _payload(await mcp.call_tool("table", {}))
            assert state["hand"] == 1, state
            for _ in range(50):
                if state.get("over"):
                    break
                kinds = {a["kind"]: a["index"] for a in state["actions"]}
                pick = kinds.get("check", kinds.get("call", state["actions"][0]["index"]))
                state = _payload(await mcp.call_tool("act", {"action": pick}))
                assert "error" not in state, state
            assert state["over"]
            assert _payload(await mcp.call_tool("score", {}))["hands"] == 1


@pytest.mark.asyncio
@pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")
async def test_each_accounts_table_is_recorded_durably(oauth_server, open_store):
    """The table an agent plays over MCP is its account's, recorded in the
    OAuth store, so it outlives the server process; each account has its
    own."""
    base, start, fake = oauth_server
    sessions = {}
    with start():
        for who, user in (("a", USER_A), ("b", USER_B)):
            fake.userinfo = user
            _, tok = await ot.sign_in_tokens(base)
            async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
                sessions[who] = (tok["access_token"], _payload(await mcp.call_tool("table", {}))["session"])
    # The server is gone; read the store a restarted one would.
    store = open_store()
    provider = LeagueOAuthProvider(store, issuer_url=base, resource_url=f"{base}/mcp")
    tables = Tables(store, TableClient())
    owner = {who: (await provider.load_access_token(tok)).subject for who, (tok, _) in sessions.items()}
    assert owner == {"a": "github:1001", "b": "github:2002"}
    sid_a, sid_b = sessions["a"][1], sessions["b"][1]
    assert sid_a != sid_b
    assert await tables.current(owner["a"]) == sid_a and await tables.current(owner["b"]) == sid_b
    assert (await store.get("table-sessions", sid_a))["owner"] == owner["a"]
    with pytest.raises(LookupError):
        await tables.resolve(owner["b"], sid_a)


@pytest.mark.asyncio
async def test_a_standard_mcp_oauth_client_finds_its_own_way(oauth_server):
    """The SDK's own OAuthClientProvider, given only the /mcp URL: it must
    discover everything from the 401 (protected-resource metadata, then
    authorization-server metadata), register, and run PKCE. The browser
    step is scripted: the GitHub button."""
    from mcp import Client
    from mcp.client.auth import OAuthClientProvider
    from mcp.client.streamable_http import streamable_http_client
    from mcp.shared.auth import AuthorizationCodeResult, OAuthClientMetadata

    class Memory:
        tokens = client_info = None

        async def get_tokens(self):
            return self.tokens

        async def set_tokens(self, tokens):
            self.tokens = tokens

        async def get_client_info(self):
            return self.client_info

        async def set_client_info(self, info):
            self.client_info = info

    base, start, _ = oauth_server
    result: dict = {}

    async def redirect_handler(authorization_url: str) -> None:
        async with httpx2.AsyncClient() as browser:
            r = await browser.get(authorization_url)
            assert r.status_code == 302
            request_id = r.headers["location"].split("request=", 1)[1]
            back = await ot.click_sign_in(browser, base, request_id)
            q = dict(httpx2.URL(back).params)
            result["code"], result["state"] = q["code"], q.get("state")

    async def callback_handler() -> AuthorizationCodeResult:
        return AuthorizationCodeResult(code=result["code"], state=result["state"])

    storage = Memory()
    auth = OAuthClientProvider(
        server_url=f"{base}/mcp",
        client_metadata=OAuthClientMetadata(redirect_uris=[ot.REDIRECT_URI], client_name="sdk client"),
        storage=storage,
        redirect_handler=redirect_handler,
        callback_handler=callback_handler,
    )
    with start():
        async with httpx2.AsyncClient(auth=auth, timeout=30) as http:
            async with Client(streamable_http_client(f"{base}/mcp", http_client=http)) as mcp:
                assert "Bot Poker" in (await mcp.call_tool("docs", {"name": "rules"})).content[0].text
    assert storage.tokens.access_token.startswith("obgl_at_")


@pytest.mark.asyncio
async def test_an_mcp_session_belongs_to_the_account_that_opened_it(oauth_server):
    base, start, fake = oauth_server
    with start():
        fake.userinfo = USER_A
        _, a = await ot.sign_in_tokens(base)
        fake.userinfo = USER_B
        _, b = await ot.sign_in_tokens(base)
        async with httpx2.AsyncClient() as http:
            r = await http.post(f"{base}/mcp", json=INIT, headers={**MCP_HEADERS, "Authorization": f"Bearer {a['access_token']}"})
            assert r.status_code == 200
            sid = r.headers["mcp-session-id"]
            ping = {"jsonrpc": "2.0", "id": 2, "method": "ping"}
            as_b = {**MCP_HEADERS, "Authorization": f"Bearer {b['access_token']}", "mcp-session-id": sid}
            assert (await http.post(f"{base}/mcp", json=ping, headers=as_b)).status_code == 404
            as_a = {**as_b, "Authorization": f"Bearer {a['access_token']}"}
            assert (await http.post(f"{base}/mcp", json=ping, headers=as_a)).status_code == 200


@pytest.mark.asyncio
async def test_tokens_survive_a_restart(oauth_server):
    base, start, _ = oauth_server
    with start():
        client, tok = await ot.sign_in_tokens(base)
    # Server stopped; a new process on the same port and the same store.
    with start():
        async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
            assert "Bot Poker" in (await mcp.call_tool("docs", {"name": "rules"})).content[0].text
        async with httpx2.AsyncClient() as http:
            r = await ot.refresh(http, base, client, tok["refresh_token"])
            assert r.status_code == 200, r.text
            new = r.json()
            # Revoke, then the endpoint refuses it.
            assert (await ot.revoke(http, base, client, new["access_token"])).status_code == 200
            r = await http.post(f"{base}/mcp", json=INIT, headers={**MCP_HEADERS, "Authorization": f"Bearer {new['access_token']}"})
            assert r.status_code == 401
