"""OAuth over real HTTP: server.py as a subprocess (OBGL_TRANSPORT=http,
OBGL_PUBLIC_URL set, so /mcp needs a bearer token), the guest sign-in
driven end to end, the MCP tools called with the token it yields, and
the file store carrying tokens across a server restart.

The table-touching test (new_session + act) skips itself if the live
table at localhost:9777/poker/ is not reachable; the rest need no table.

    python -m pytest mcp/test_oauth_http.py -q
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2
import pytest

import oauth_testing as ot
from table_client import TableClient


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


@pytest.fixture
def oauth_server(run_server, free_port, tmp_path):
    """A server with OAuth on and a file store; yields (base, start) where
    start() is a context manager running it on the same port and file
    (call it again after the first exits: a restart)."""
    base = f"http://127.0.0.1:{free_port}"

    def start():
        return run_server(
            port=free_port,
            OBGL_PUBLIC_URL=base,
            OBGL_AUTH_STORE="file",
            OBGL_AUTH_FILE=str(tmp_path / "oauth.json"),
        )

    return base, start


def _payload(result):
    assert not result.is_error, result.content
    if result.structured_content is not None:
        return result.structured_content
    return json.loads(result.content[0].text)


@pytest.mark.asyncio
async def test_mcp_needs_a_token_and_says_where_to_get_one(oauth_server):
    base, start = oauth_server
    with start():
        async with httpx2.AsyncClient() as http:
            r = await http.post(f"{base}/mcp", json=INIT, headers=MCP_HEADERS)
            assert r.status_code == 401
            assert f'resource_metadata="{base}/.well-known/oauth-protected-resource/mcp"' in r.headers["www-authenticate"]
            r = await http.post(f"{base}/mcp", json=INIT, headers={**MCP_HEADERS, "Authorization": "Bearer obgl_at_nope"})
            assert r.status_code == 401
            # The docs and the landing page stay public.
            assert (await http.get(f"{base}/docs/AGENTS.md")).status_code == 200
            assert "Continue as guest" in (await http.get(f"{base}/")).text


@pytest.mark.asyncio
async def test_guest_sign_in_then_tools(oauth_server):
    base, start = oauth_server
    with start():
        _, tok = await ot.guest_tokens(base)
        async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
            assert "Bot Poker" in (await mcp.call_tool("docs", {"name": "rules"})).content[0].text
            assert "new_session" in _payload(await mcp.call_tool("state", {}))["error"]


@pytest.mark.asyncio
@pytest.mark.skipif(not _table_up(), reason="live table not reachable at localhost:9777")
async def test_guest_plays_a_hand(oauth_server):
    base, start = oauth_server
    with start():
        _, tok = await ot.guest_tokens(base)
        async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
            state = _payload(await mcp.call_tool("new_session", {}))
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
async def test_a_standard_mcp_oauth_client_finds_its_own_way(oauth_server):
    """The SDK's own OAuthClientProvider, given only the /mcp URL: it must
    discover everything from the 401 (protected-resource metadata, then
    authorization-server metadata), register, and run PKCE. The browser
    step is scripted: the guest button."""
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

    base, start = oauth_server
    result: dict = {}

    async def redirect_handler(authorization_url: str) -> None:
        async with httpx2.AsyncClient() as browser:
            r = await browser.get(authorization_url)
            assert r.status_code == 302
            request_id = r.headers["location"].split("request=", 1)[1]
            r = await browser.post(f"{base}/login/guest", data={"request": request_id})
            assert r.status_code == 303
            q = dict(httpx2.URL(r.headers["location"]).params)
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
                assert "new_session" in _payload(await mcp.call_tool("state", {}))["error"]
    assert storage.tokens.access_token.startswith("obgl_at_")


@pytest.mark.asyncio
async def test_an_mcp_session_belongs_to_the_account_that_opened_it(oauth_server):
    base, start = oauth_server
    with start():
        _, a = await ot.guest_tokens(base)
        _, b = await ot.guest_tokens(base)
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
async def test_tokens_survive_a_restart_with_the_file_store(oauth_server):
    base, start = oauth_server
    with start():
        client, tok = await ot.guest_tokens(base)
    # Server stopped; a new process on the same port and the same file.
    with start():
        async with ot.mcp_client(f"{base}/mcp", tok["access_token"]) as mcp:
            assert "new_session" in _payload(await mcp.call_tool("state", {}))["error"]
        async with httpx2.AsyncClient() as http:
            r = await ot.refresh(http, base, client, tok["refresh_token"])
            assert r.status_code == 200, r.text
            new = r.json()
            # Revoke, then the endpoint refuses it.
            assert (await ot.revoke(http, base, client, new["access_token"])).status_code == 200
            r = await http.post(f"{base}/mcp", json=INIT, headers={**MCP_HEADERS, "Authorization": f"Bearer {new['access_token']}"})
            assert r.status_code == 401
