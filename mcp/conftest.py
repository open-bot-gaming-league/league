"""Shared pytest fixtures for the HTTP-transport tests."""

import os
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from contextlib import contextmanager

import pytest

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
