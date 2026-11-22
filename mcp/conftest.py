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
def _running_server(**env_extra: str):
    """Starts server.py (OBGL_TRANSPORT=http) bound to a free local port
    and yields its base URL (no path), e.g. http://127.0.0.1:41234.
    Extra keyword arguments are added to the server's environment
    (OBGL_TABLE_URL is left at its default, the live table at
    localhost:9777/poker/, unless given). Waits for /mcp to answer
    before yielding; stops the process on exit."""
    port = _free_port()
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


@pytest.fixture
def run_server():
    """The context manager above, as a fixture:
    `with run_server(OBGL_TABLE_URL="...") as base: ...`."""
    return _running_server
