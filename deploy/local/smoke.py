#!/usr/bin/env python3
"""smoke.py: prove a running OBGL stack answers, over HTTP only.

  smoke.py --url http://127.0.0.1:9792 --repo DIR [--sha SHA] [--require oauth,version,docs]

Needs the `mcp` package (run it with the venv python run.sh built, or any
venv with mcp==2.3.0). Checks, printing one line each:
  oauth    if the server advertises OAuth (GET /.well-known/oauth-protected-resource/mcp):
           /mcp without a token is 401, then a guest token through the real flow --
           dynamic registration, /authorize with PKCE, the sign-in page, the guest
           button, /token (DIR/mcp/oauth_testing.py) -- used by every mcp step below
  mcp      raw JSON-RPC `initialize` POST to /mcp (shows the protocol version
           and the Mcp-Session-Id header), then, with the MCP client:
           new_session(turtle) and one act() on its first legal action
  version  GET /version == {"sha": SHA}        (only if --sha)
  docs     GET /docs/AGENTS.md == DIR/docs/AGENTS.md, byte for byte
A check whose route is missing (404) prints "absent" and passes unless it is
named in --require. Exit 1 on any failure.
"""

import argparse
import asyncio
import json
import os
import sys
import urllib.error
import urllib.request


def get(url):
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def raw_initialize(base, token=None):
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(
        base + "/mcp",
        data=json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "initialize",
            "params": {"protocolVersion": "2025-06-18", "capabilities": {},
                       "clientInfo": {"name": "obgl-local-smoke", "version": "0"}},
        }).encode(),
        headers=headers,
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=10) as r:
        body = r.read().decode()
        sid = r.headers.get("mcp-session-id")
    # The reply is JSON or one SSE `data:` line, depending on the server.
    data = next((l[5:].strip() for l in body.splitlines() if l.startswith("data:")), body)
    result = json.loads(data)["result"]
    return result["protocolVersion"], result["serverInfo"]["name"], sid


def guest_token(base, repo):
    """A guest access token through the server's real OAuth flow, or None
    if this sha has no OAuth (no protected-resource metadata)."""
    status, _, body = get(base + "/.well-known/oauth-protected-resource/mcp")
    if status == 404:
        return None
    prm = json.loads(body)
    try:
        raw_initialize(base)
        raise AssertionError("OAuth is advertised but /mcp answered without a token")
    except urllib.error.HTTPError as e:
        assert e.code == 401, e.code
        print(f"oauth    /mcp without a token: 401, WWW-Authenticate {e.headers.get('www-authenticate')}")
    sys.path.insert(0, os.path.join(repo, "mcp"))
    import oauth_testing

    client, tok = asyncio.run(oauth_testing.guest_tokens(base))
    print(f"oauth    guest sign-in: resource {prm['resource']}, client {client['client_id']} registered, "
          f"code -> token {tok['access_token'][:12]}... (expires_in {tok['expires_in']}, refresh token "
          f"{'yes' if tok.get('refresh_token') else 'no'})")
    return tok["access_token"]


def _client(base, token):
    if token is None:
        from mcp import Client

        return Client(base + "/mcp")
    import oauth_testing

    return oauth_testing.mcp_client(base + "/mcp", token)


async def play(base, token):
    def payload(res):
        assert not res.is_error, res.content
        return res.structured_content if res.structured_content is not None else json.loads(res.content[0].text)

    async with _client(base, token) as c:
        rules = payload(await c.call_tool("rules", {}))
        bots = [b["id"] for b in rules["bots"]]
        st = payload(await c.call_tool("new_session", {"opponent": "turtle", "seat": "alternate"}))
        assert "error" not in st, st
        print(f"mcp      new_session(turtle): session {st['session']} hand {st['hand']}, bots on offer {bots}")
        # check/call if offered (so the hand goes on), else whatever is first
        first = next((x for x in st["actions"] if x["kind"] in ("check", "call")), st["actions"][0])
        st2 = payload(await c.call_tool("act", {"action": first["index"]}))
        assert "error" not in st2, st2
        print(f"mcp      act({first['index']}={first['kind']}): step {st['step']} -> {st2['step']}, over={st2.get('over')}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--repo", required=True, help="checkout the server was built from")
    ap.add_argument("--sha")
    ap.add_argument("--require", default="")
    a = ap.parse_args()
    base, required, failed = a.url.rstrip("/"), set(filter(None, a.require.split(","))), False

    def check(name, ok, detail):
        nonlocal failed
        print(f"{name:8} {'ok' if ok else 'FAIL'}: {detail}" if ok is not None else f"{name:8} absent (404): {detail}")
        if ok is False or (ok is None and name in required):
            failed = True

    token = guest_token(base, a.repo)
    if token is None:
        check("oauth", None, "GET /.well-known/oauth-protected-resource/mcp")
    proto, server, sid = raw_initialize(base, token)
    print(f"mcp      initialize: protocol {proto}, server {server!r}, Mcp-Session-Id {sid}"
          f"{', with the guest bearer token' if token else ''}")
    asyncio.run(play(base, token))

    if a.sha:
        status, _, body = get(base + "/version")
        if status == 404:
            check("version", None, "GET /version")
        else:
            got = json.loads(body).get("sha") if status == 200 else None
            check("version", got == a.sha, f"GET /version -> {status} {body.decode().strip()}")

    status, hdrs, body = get(base + "/docs/AGENTS.md")
    if status == 404:
        check("docs", None, "GET /docs/AGENTS.md")
    else:
        want = open(os.path.join(a.repo, "docs", "AGENTS.md"), "rb").read()
        check("docs", status == 200 and body == want,
              f"GET /docs/AGENTS.md -> {status} {hdrs.get('content-type')}, {len(body)} bytes, "
              f"{'identical to' if body == want else 'DIFFERENT from'} {a.repo}/docs/AGENTS.md ({len(want)} bytes)")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
