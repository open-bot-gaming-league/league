"""Shared pytest fixtures for the HTTP-transport tests: run_server (server.py
as a subprocess), serve_asgi (an app in-process on a socket), and site (our
server in-process with a fake GitHub, for the browser table's tests; once
on the memory store, and once on Firestore when an emulator is up)."""

import os
import socket
import subprocess
import sys
import time
import urllib.error
import uuid
import urllib.request
from contextlib import asynccontextmanager, contextmanager

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import httpx2  # noqa: E402

import oauth_testing as ot  # noqa: E402
import server as server_module  # noqa: E402
from oauth import OAuthConfig  # noqa: E402
from oauth.idp import github  # noqa: E402
from oauth.store import FirestoreStore, MemoryStore  # noqa: E402

USER_A = {"id": 1001, "login": "alice"}  # GitHub users for the fake IdP (the `site` fixture)
USER_B = {"id": 2002, "login": "bob"}

SERVER_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "server.py")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@contextmanager
def _running_server(port: int | None = None, **env_extra: str):
    """Starts server.py (OBGL_TRANSPORT=http) bound to `port` (default: a
    free local port) and yields its base URL (no path), e.g.
    http://127.0.0.1:41234. Extra keyword arguments are added to the
    server's environment (OBGL_TABLE_URL is left at its default, the live
    table at localhost:9777/poker/, unless given). Pass the port to start
    a second server where a first one was (a restart), or to know the URL
    before start (OBGL_PUBLIC_URL). Waits for /mcp to answer before
    yielding; stops the process on exit."""
    port = port or _free_port()
    env = dict(os.environ)
    env["OBGL_TRANSPORT"] = "http"
    env["PORT"] = str(port)
    env["HOST"] = "127.0.0.1"
    env.update(env_extra)
    proc = subprocess.Popen([sys.executable, SERVER_PATH], env=env)
    base = f"http://127.0.0.1:{port}"
    try:
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                req = urllib.request.Request(
                    f"{base}/mcp",
                    data=b"{}",
                    headers={"Content-Type": "application/json", "Accept": "application/json, text/event-stream"},
                    method="POST",
                )
                urllib.request.urlopen(req, timeout=1)
                break
            except urllib.error.HTTPError:
                break  # server answered (even an error response means it's up)
            except Exception:
                time.sleep(0.2)
        else:
            raise RuntimeError("server.py did not come up on /mcp within 10s")
        yield base
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


@contextmanager
def _serving_asgi(app, port: int | None = None):
    """Serves an ASGI app in-process (uvicorn on a thread) on `port`
    (default: a free one); yields its base URL. For tests that need a
    real socket but an app built in the test (the fake upstream IdP, a
    server wired to it)."""
    import threading

    import uvicorn

    port = port or _free_port()
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning", lifespan="on"))
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    deadline = time.monotonic() + 10
    while not server.started:
        if time.monotonic() > deadline or not thread.is_alive():
            raise RuntimeError("in-process server did not start")
        time.sleep(0.05)
    try:
        yield f"http://127.0.0.1:{port}"
    finally:
        server.should_exit = True
        thread.join(timeout=10)


@pytest.fixture
def free_port():
    return _free_port()


@pytest.fixture
def serve_asgi():
    """The context manager above, as a fixture: `with serve_asgi(app) as base: ...`."""
    return _serving_asgi


@pytest.fixture
def run_server():
    """The context manager above, as a fixture:
    `with run_server(OBGL_TABLE_URL="...") as base: ...`."""
    return _running_server


class Site:
    """Our server with a fake GitHub; `browser(user)` and `agent(user)`
    sign in as that GitHub user."""

    def __init__(self, base, fake):
        self.base, self.fake = base, fake

    @asynccontextmanager
    async def browser(self, user):
        async with httpx2.AsyncClient(base_url=self.base, timeout=30) as http:
            self.fake.userinfo = user
            landed = await ot.table_sign_in(http, self.base, "github")
            assert landed == "/table/", landed
            yield http

    async def agent_token(self, user):
        self.fake.userinfo = user
        return (await ot.agent_tokens(self.base, "github"))["access_token"]


def league_app(base, idp_base, store):
    """Our server's ASGI app at `base` with OAuth on and a GitHub sign-in
    wired to the fake IdP served at `idp_base`."""
    idp = github(
        ot.UPSTREAM_ID,
        ot.UPSTREAM_SECRET,
        f"{base}/login/github/callback",
        authorize_endpoint=f"{idp_base}/authorize",
        token_endpoint=f"{idp_base}/token",
        userinfo_endpoint=f"{idp_base}/userinfo",
    )
    config = OAuthConfig(public_url=base, mcp_path="/mcp", store=store, idps=[idp])
    return server_module.build_server(None, config).streamable_http_app()


def _site_store(kind):
    """The `site` fixture's store: memory, or Firestore (a fresh, empty
    emulator project) when an emulator is up."""
    if kind == "memory":
        return MemoryStore()
    if not os.environ.get("FIRESTORE_EMULATOR_HOST"):
        pytest.skip("FIRESTORE_EMULATOR_HOST not set (no Firestore emulator)")
    return FirestoreStore(f"demo-obgl-{uuid.uuid4().hex[:12]}")


@pytest.fixture(params=["memory", "firestore"])
def site(request, serve_asgi, free_port):
    store = _site_store(request.param)
    fake = ot.FakeIdP(USER_A)
    with serve_asgi(fake.app) as idp_base:
        base = f"http://127.0.0.1:{free_port}"
        with serve_asgi(league_app(base, idp_base, store), free_port):
            yield Site(base, fake)
