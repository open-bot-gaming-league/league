"""GET /version: the server reports the git sha it was built from.

In-process (Starlette's test client on the server's own ASGI app), so it
needs neither the table nor a subprocess. Run: python -m pytest mcp/test_version.py -q
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from starlette.testclient import TestClient

import server as server_module

SHA = "f6b6e19b0a1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e"


def _get_version(monkeypatch, sha):
    if sha is None:
        monkeypatch.delenv("OBGL_GIT_SHA", raising=False)
    else:
        monkeypatch.setenv("OBGL_GIT_SHA", sha)
    app = server_module.build_server("http://127.0.0.1:1/").streamable_http_app()
    return TestClient(app).get("/version")


def test_version_returns_the_sha_from_the_environment(monkeypatch):
    r = _get_version(monkeypatch, SHA)
    assert r.status_code == 200
    assert r.headers["content-type"].startswith("application/json")
    assert r.json() == {"sha": SHA}


def test_version_says_unknown_when_nobody_set_it(monkeypatch):
    for unset in (None, ""):
        r = _get_version(monkeypatch, unset)
        assert r.status_code == 200
        assert r.json() == {"sha": "unknown"}
