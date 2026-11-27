"""Tests for the docs layers: the /docs/* web pages and the docs() tool.

Neither needs the table, so these start server.py with OBGL_TABLE_URL
pointing at a closed port: they pass (and prove it) with no table at
all. Run: python -m pytest mcp/test_docs.py -q
"""

import os
import sys
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

import server as server_module

REPO = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
FILES = {
    "/docs/README.md": os.path.join(REPO, "README.md"),
    "/docs/AGENTS.md": os.path.join(REPO, "docs", "AGENTS.md"),
    "/docs/WAYS-TO-PLAY.md": os.path.join(REPO, "docs", "WAYS-TO-PLAY.md"),
}
FILES["/docs/PRIVACY.md"] = os.path.join(REPO, "docs", "PRIVACY.md")
FILES["/docs/TERMS.md"] = os.path.join(REPO, "docs", "TERMS.md")
FILES["/docs/SDK.md"] = os.path.join(REPO, "docs", "SDK.md")
# The legal pages: URL path -> (a heading the page must show, the markdown file's name).
# Each legal page's stated date, as its file has it: a change to the text changes it.
UPDATED = {"/docs/privacy": "November 27, 2026", "/docs/terms": "November 27, 2026"}
LEGAL = {
    "/docs/privacy": ("Privacy (interim)", "/docs/PRIVACY.md"),
    "/docs/terms": ("Terms of Service (interim)", "/docs/TERMS.md"),
}
TOOL_NAMES = {
    "readme": FILES["/docs/README.md"],
    "agents": FILES["/docs/AGENTS.md"],
    "ways-to-play": FILES["/docs/WAYS-TO-PLAY.md"],
    "privacy": FILES["/docs/PRIVACY.md"],
    "terms": FILES["/docs/TERMS.md"],
    "sdk": FILES["/docs/SDK.md"],
}


@pytest.fixture
def base(run_server):
    with run_server(OBGL_TABLE_URL="http://127.0.0.1:1/") as b:
        yield b


def _get(url):
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read()


@pytest.mark.parametrize("path", sorted(FILES))
def test_doc_file_is_served_as_exact_bytes(base, path):
    status, ctype, body = _get(base + path)
    assert status == 200
    assert ctype.startswith("text/markdown")
    with open(FILES[path], "rb") as f:
        assert body == f.read()


def test_docs_index_links_each_doc(base):
    status, ctype, body = _get(base + "/docs/")
    assert status == 200 and ctype.startswith("text/html")
    for path in [*FILES, *LEGAL]:
        assert f'href="{path}"'.encode() in body


def test_docs_index_without_trailing_slash_answers_directly(base):
    import http.client

    conn = http.client.HTTPConnection(base.removeprefix("http://"), timeout=5)
    conn.request("GET", "/docs")  # http.client does not follow redirects
    resp = conn.getresponse()
    assert resp.status == 200 and resp.getheader("Content-Type").startswith("text/html")
    conn.close()


def test_landing_page_has_connector_url_and_docs_link(base):
    status, ctype, body = _get(base + "/")
    assert status == 200 and ctype.startswith("text/html")
    assert server_module.CONNECTOR_URL.encode() in body
    assert b'href="/docs/"' in body
    for path in LEGAL:
        assert f'href="{path}"'.encode() in body


@pytest.mark.parametrize("path", sorted(LEGAL))
def test_legal_page_is_html_with_its_heading(base, path):
    heading, _ = LEGAL[path]
    status, ctype, body = _get(base + path)
    assert status == 200 and ctype.startswith("text/html")
    page = body.decode()
    assert page.startswith("<!doctype html>")
    assert f"<title>{heading}</title>" in page
    assert f"<h1>{heading}</h1>" in page
    assert "<script" not in page


@pytest.mark.parametrize("path", sorted(LEGAL))
def test_legal_page_links_home_and_the_other_legal_page(base, path):
    page = _get(base + path)[2].decode()
    assert 'href="/"' in page
    for other in LEGAL:
        assert (f'href="{other}"' in page) == (other != path)


@pytest.mark.parametrize("path", sorted(LEGAL))
def test_legal_pages_carry_no_drafting_markers(base, path):
    """Public repo and public page: no placeholders or review notes, in the
    served page or in the markdown it is rendered from."""
    for text in (_get(base + path)[2].decode(), _get(base + LEGAL[path][1])[2].decode()):
        for marker in ("[CHECK", "DRAFT", "PLACEHOLDER", "[EFFECTIVE DATE]"):
            assert marker not in text, (path, marker)
        assert f"Last updated: {UPDATED[path]}" in text


def test_unknown_doc_is_404(base):
    assert _get(base + "/docs/NOPE.md")[0] == 404


def test_doc_urls_in_readme_and_agents_resolve_to_served_paths():
    """README/AGENTS point at https://obgl.truemirrorgames.com/docs/...:
    every such URL must be a path this server actually serves."""
    import re

    served = set(FILES) | set(LEGAL) | {"/docs/"}
    for path in (FILES["/docs/README.md"], FILES["/docs/AGENTS.md"]):
        with open(path, encoding="utf-8") as f:
            text = f.read()
        for url_path in re.findall(r"https://obgl\.truemirrorgames\.com(/docs/[^\s)]*)", text):
            assert url_path in served, (path, url_path)


@pytest.mark.asyncio
@pytest.mark.parametrize("name", sorted(TOOL_NAMES))
async def test_docs_tool_returns_file_text(base, name):
    from mcp import Client

    async with Client(f"{base}/mcp") as client:
        result = await client.call_tool("docs", {"name": name})
    assert not result.is_error, result.content
    with open(TOOL_NAMES[name], encoding="utf-8") as f:
        assert result.content[0].text == f.read()


@pytest.mark.asyncio
async def test_docs_tool_rules_is_the_servers_rules_text(base):
    from mcp import Client

    async with Client(f"{base}/mcp") as client:
        result = await client.call_tool("docs", {"name": "rules"})
    assert not result.is_error, result.content
    assert result.content[0].text == server_module.RULES_TEXT


@pytest.mark.asyncio
async def test_docs_tool_unknown_name_is_a_clear_error(base):
    from mcp import Client

    async with Client(f"{base}/mcp") as client:
        result = await client.call_tool("docs", {"name": "nope"})
    assert result.is_error
    message = result.content[0].text
    assert "nope" in message
    for name in server_module.DOC_NAMES:
        assert name in message


@pytest.mark.asyncio
async def test_instructions_say_to_call_docs_agents_first(base):
    from mcp import Client

    async with Client(f"{base}/mcp") as client:
        instructions = client.instructions
    assert instructions and "docs('agents')" in instructions


def test_the_agent_is_told_where_the_real_answer_to_how_good_is():
    """Asked 'what's a good score?' / 'are we winning?' / 'who's the
    best?', the agent points to ranked play: build as wasm, bot submit,
    the queue's table, table_results with its badges."""
    with open(FILES["/docs/AGENTS.md"], encoding="utf-8") as f:
        agents = f.read()
    section = agents[agents.index("### When your user asks how good their bot is"):]
    section = section[:section.index("\n#", 1)]
    for phrase in ("what's a good score?", "are we winning?", "who's the", "ranked table", "wasm", "`bot submit`",
                   "`table_results()`", "badges", "follow \"Ranked play\""):
        assert phrase in section, phrase
    with open(os.path.join(REPO, "docs", "SDK.md"), encoding="utf-8") as f:
        sdk = f.read()
    assert "7. **Ranked play.**" in sdk and "Submit your\n   bot" in sdk and "badges" in sdk
    assert "When your user asks \"what's a good score?\"" in sdk


@pytest.mark.asyncio
async def test_instructions_point_how_good_questions_to_ranked_play(base):
    from mcp import Client

    async with Client(f"{base}/mcp") as client:
        instructions = client.instructions
    for phrase in ("what a good score is", "whether they are winning", "who is the best", "a ranked table is the real answer",
                   "bot submit", "table_results()", "badges", "'When your user asks how good their bot is'"):
        assert phrase in instructions, phrase
