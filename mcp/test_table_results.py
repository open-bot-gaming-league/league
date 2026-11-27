"""table_results() and /table/results/<id>: a ranked table's results, from
the Go table (cmd/table, api/results and results/<id>). In-process, with a
fake Go table that answers as the real one does for the first full-size
ranked table: its text is the Go report's own, the golden file the Go
tests check (poker/engine/league/testdata/table-p8.txt)."""

import json
import os
import sys
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest
from mcp import Client

import oauth_testing as ot
import server as server_module
from oauth import OAuthConfig
from oauth.idp import github
from oauth.store import MemoryStore

GOLDEN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "poker", "engine", "league", "testdata", "table-p8.txt")
with open(GOLDEN, encoding="utf-8") as f:
    TEXT = f.read()
PAGE = b"<!doctype html><html><head></head><body><h1>Example placed 10th of 10</h1></body></html>"


class FakeTable(BaseHTTPRequestHandler):
    """The Go table's results routes, for the one table it keeps."""

    def do_GET(self):  # noqa: N802
        if self.path == "/api/results/table-p8":
            self._send(200, "application/json", json.dumps(
                {"id": "table-p8", "placed": "10th of 10", "summary": "beat 5 of 9, lost to 4, too close to call 0", "text": TEXT}).encode())
        elif self.path == "/results/table-p8":
            self._send(200, "text/html; charset=utf-8", PAGE)
        elif self.path.startswith("/api/results/"):
            name = urllib.request.unquote(self.path.removeprefix("/api/results/"))
            self._send(404, "application/json", json.dumps({"error": f"no ranked table {name!r}"}).encode())
        else:
            self._send(404, "text/plain; charset=utf-8", b"no ranked table\n")

    def _send(self, status, ctype, body):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


# In-process clients connect "legacy": the server opts out of the modern
# handshake (server.py, _discover_legacy_only).


@pytest.fixture
def table_url():
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), FakeTable)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{httpd.server_address[1]}/"
    httpd.shutdown()
    thread.join(timeout=5)


def _oauth(base):
    idp = github(ot.UPSTREAM_ID, ot.UPSTREAM_SECRET, f"{base}/login/github/callback",
                 authorize_endpoint="http://127.0.0.1:1/authorize", token_endpoint="http://127.0.0.1:1/token",
                 userinfo_endpoint="http://127.0.0.1:1/userinfo")
    return OAuthConfig(public_url=base, mcp_path="/mcp", store=MemoryStore(), idps=[idp])


@pytest.mark.asyncio
async def test_table_results_reads_the_report_aloud(table_url):
    async with Client(server_module.build_server(table_url), mode="legacy") as client:
        result = await client.call_tool("table_results", {"table_id": "table-p8"})
    assert not result.is_error, result.content
    text = result.content[0].text
    assert text == TEXT  # no sign-in, no page: the report's text as the table wrote it
    lines = text.splitlines()
    assert lines[0] == "Example placed 10th of 10, -187.0 bb/100 summed over its 9 opponents."
    assert lines[1] == "It beat 5 of 9, lost to 4, too close to call 0."
    rows = [line.split() for line in lines if line.endswith((" won", " lost", " too close to call"))]
    assert len(rows) == 9
    assert sum(r[-1] == "won" for r in rows) == 5 and sum(r[-1] == "lost" for r in rows) == 4
    assert "  10. Example" in text and "<- Example" in text
    assert "exploit" not in text.lower()
    assert len(text) < 2000  # short enough to read aloud


@pytest.mark.asyncio
async def test_table_results_unknown_table_is_a_clear_error(table_url):
    async with Client(server_module.build_server(table_url), mode="legacy") as client:
        result = await client.call_tool("table_results", {"table_id": "nope"})
    assert result.is_error
    assert "no ranked table 'nope'" in result.content[0].text


@pytest.mark.asyncio
async def test_with_sign_in_the_tool_links_the_page_and_the_page_is_open(table_url, serve_asgi, free_port):
    base = f"http://127.0.0.1:{free_port}"
    server = server_module.build_server(table_url, _oauth(base))
    async with Client(server, mode="legacy") as client:
        result = await client.call_tool("table_results", {"table_id": "table-p8"})
    assert not result.is_error, result.content
    assert result.content[0].text == TEXT + f"\nThe full report, with every pair: {base}/table/results/table-p8\n"
    with serve_asgi(server.streamable_http_app(), free_port):
        # No sign-in: the page is anyone's with the link.
        with urllib.request.urlopen(f"{base}/table/results/table-p8", timeout=10) as r:
            assert r.status == 200 and r.read() == PAGE
            assert r.headers["Content-Type"].startswith("text/html")
            assert "frame-ancestors 'none'" in r.headers["Content-Security-Policy"]
        with pytest.raises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(f"{base}/table/results/nope", timeout=10)
        assert e.value.code == 404 and b"No such table" in e.value.read()
