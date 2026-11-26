#!/usr/bin/env python3
"""smoke.py: prove a running OBGL stack answers, over HTTP only.

  smoke.py --url http://127.0.0.1:9792 --repo DIR [--sha SHA] [--require oauth,version,docs] [--sign-in IDP] [--stay-on-base]

Needs the `mcp` package (run it with the venv python run.sh built, or any
venv with mcp==2.3.0). Checks, printing one line each:
  oauth    if the server advertises OAuth (GET /.well-known/oauth-protected-resource/mcp):
           /mcp without a token is 401; dynamic registration and /authorize with PKCE
           lead to the sign-in page, which offers no guest sign-in (POST /login/guest
           is 404), keeps its CSP, links the Terms and Privacy pages, and whose Cancel
           sends access_denied back to the client. With --sign-in: then a token
           through the real flow (the sign-in page's IDP button, /token;
           DIR/mcp/oauth_testing.py), used by every mcp step below
  mcp      (with OAuth: only with --sign-in) raw JSON-RPC `initialize` POST to /mcp (shows the protocol version
           and the Mcp-Session-Id header), then, with the MCP client:
           table(), one act() on its first legal action, and (a sha with
           any-size betting) one off-menu raise by kind and amount, played to the end
  version  GET /version == {"sha": SHA}        (only if --sha)
  docs     GET /docs/AGENTS.md == DIR/docs/AGENTS.md, byte for byte; and GET /docs/privacy
           and /docs/terms are 200 text/html (the pages the sign-in screens link to)
  table    GET /table/api/info is 200 and lists the bots (public: no sign-in). Then the
           browser table at /table/ (with OAuth: only with --sign-in): a browser
           (a cookie jar) signs in through /table/signin and the sign-in page, gets the page,
           reads its table, resets it and acts; makes a program token, reads the same
           table with it, revokes it (then 401); without the cookie the API answers 401
--sign-in IDP: click that IdP's button ("github") on the sign-in page. Only a server
wired to a test IdP (mcp/oauth_testing.FakeIdP, as in mcp/test_smoke.py) lets a script
through it; a real deployment or the local stack (run.sh) has no such IdP, so there the
signed-in steps (mcp, table) are skipped, and printed as skipped. The table step
calls api/token/regenerate, which revokes that account's existing program token:
use --sign-in against a test IdP's account only, never a real person's.
A check whose route is missing (404) prints "absent" and passes unless it is
named in --require. Exit 1 on any failure.

--stay-on-base: the table check's browser follows redirects to the OAuth issuer
(the server's OBGL_PUBLIC_URL) on --url's origin instead. Use it when --url is not
the issuer, e.g. a Cloud Run revision tagged with --no-traffic, so every request
goes to the revision under test.
"""

import argparse
import asyncio
import http.cookiejar
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from urllib.parse import parse_qs, urlencode, urlparse


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


def protected_resource(base):
    """The server's protected-resource metadata, or None without OAuth."""
    status, _, body = get(base + "/.well-known/oauth-protected-resource/mcp")
    return None if status == 404 else json.loads(body)


def sign_in_page_check(base, repo, prm):
    """The unauthenticated half of OAuth, through the real routes: /mcp without a
    token is 401; register a client, /authorize, read the sign-in page, then
    Cancel. Returns (client, resource) for a sign-in that follows."""
    try:
        raw_initialize(base)
        raise AssertionError("OAuth is advertised but /mcp answered without a token")
    except urllib.error.HTTPError as e:
        assert e.code == 401, e.code
        print(f"oauth    /mcp without a token: 401, WWW-Authenticate {e.headers.get('www-authenticate')}")
    sys.path.insert(0, os.path.join(repo, "mcp"))
    import httpx2
    import oauth_testing

    async def go():
        async with httpx2.AsyncClient(timeout=10) as http:
            client = await oauth_testing.register(http, base)
            _, challenge = oauth_testing.pkce_pair()
            request_id = await oauth_testing.start_authorize(
                http, base, client, challenge=challenge, state="smoke", resource=prm["resource"])
            page = await http.get(f"{base}/login", params={"request": request_id})
            assert page.status_code == 200, page.status_code
            text = page.text
            assert "guest" not in text.lower(), "the sign-in page still mentions guests"
            csp = page.headers["content-security-policy"]
            assert "default-src 'none'" in csp and "script-src" not in csp and "<script" not in text, csp
            for link in ("/docs/terms", "/docs/privacy"):
                assert f'href="{link}"' in text, f"the sign-in page lacks the {link} link"
            guest = await http.post(f"{base}/login/guest", data={"request": request_id})
            assert guest.status_code == 404, f"POST /login/guest -> {guest.status_code}"
            cancel = await http.post(f"{base}/login/cancel", data={"request": request_id})
            assert cancel.status_code == 303 and "error=access_denied" in cancel.headers["location"], cancel.headers
            buttons = re.findall(r"Continue with (\w+)", text)
            print(f"oauth    sign-in page: buttons {', '.join(buttons) or 'none'}, no guest, CSP kept, terms and privacy linked; "
                  f"POST /login/guest 404; Cancel -> access_denied")

    asyncio.run(go())


def sign_in_token(base, repo, prm, idp):
    """An access token through the server's real OAuth flow, signing in with
    the `idp` button, for the resource the server names (the issuer's /mcp)."""
    import oauth_testing

    client, tok = asyncio.run(oauth_testing.sign_in_tokens(base, idp, resource=prm["resource"]))
    print(f"oauth    {idp} sign-in: resource {prm['resource']}, client {client['client_id']} registered, "
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
        st = payload(await c.call_tool("table", {}))
        assert "error" not in st, st
        while st["over"]:  # a table someone played before: on to a live decision
            st = payload(await c.call_tool("next_hand", {}))
            assert "error" not in st, st
        print(f"mcp      table(): session {st['session']} hand {st['hand']} ({st['opponent']}), bots on offer {bots}")
        # check/call if offered (so the hand goes on), else whatever is first
        first = next((x for x in st["actions"] if x["kind"] in ("check", "call")), st["actions"][0])
        st2 = payload(await c.call_tool("act", {"action": first["index"]}))
        assert "error" not in st2, st2
        print(f"mcp      act({first['index']}={first['kind']}): step {st['step']} -> {st2['step']}, over={st2.get('over')}")
        # Any bet size (a sha with real sizing has `legal`): one off-menu
        # wager by kind and amount, then check/call the hand to its end,
        # so the bot answers an off-menu size through translation.
        st = st2
        for _ in range(10):  # to a decision of ours with a wager open
            if st.get("over"):
                st = payload(await c.call_tool("next_hand", {}))
                continue
            if "legal" not in st:
                print("mcp      sizing absent: this sha's state has no `legal` (menu-only)")
                return
            if st["legal"].get("wager"):
                break
            st = payload(await c.call_tool("act", {"action": "check" if st["legal"]["check"] else "call"}))
        legal, menu = st["legal"], {a.get("amount") for a in st["actions"]}
        assert legal.get("wager"), st
        amount = next(x for x in range(legal["minTo"] + (legal["maxTo"] - legal["minTo"]) // 7, legal["maxTo"]) if x not in menu)
        st2 = payload(await c.call_tool("act", {"action": legal["wager"], "amount": amount}))
        assert "error" not in st2, st2
        mine = [line for line in st2["lines"] if line.startswith("You ")][-1]
        assert mine.endswith(f" {amount}") or f" {amount}," in mine, (mine, amount)
        print(f"mcp      act({legal['wager']!r}, {amount}) off the menu (legal {legal['minTo']}-{legal['maxTo']}): \"{mine}\", step {st['step']} -> {st2['step']}")
        st, moves = st2, 0
        while not st.get("over") and moves < 20:
            legal = st["legal"]
            st = payload(await c.call_tool("act", {"action": "check" if legal["check"] else "call"}))
            assert "error" not in st, st
            moves += 1
        assert st.get("over"), st
        print(f"mcp      hand {st['hand']} played out: {st['outcome']}")


class StayOnBase(urllib.request.HTTPRedirectHandler):
    """Follows a redirect to `issuer`'s origin on `base`'s origin instead."""

    def __init__(self, issuer, base):
        self.issuer, self.base = urlparse(issuer), urlparse(base)

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        u = urlparse(urllib.parse.urljoin(req.full_url, newurl))
        if (u.scheme, u.netloc) == (self.issuer.scheme, self.issuer.netloc):
            newurl = u._replace(scheme=self.base.scheme, netloc=self.base.netloc).geturl()
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def table_info(base):
    """The table's public info route, no sign-in: (ok, detail), or None if /table is absent."""
    status, _, body = get(base + "/table/api/info")
    if status == 404:
        return None
    bots = [b["id"] for b in json.loads(body).get("bots", [])] if status == 200 else []
    return status == 200 and bool(bots), f"GET /table/api/info -> {status}, bots {', '.join(bots) or 'none'} (no sign-in)"


def table(base, idp, issuer=None):
    """The browser table, as a browser signed in with `idp`: (ok, detail).
    With `issuer`, redirects to the issuer's origin stay on `base` (--stay-on-base)."""
    jar = http.cookiejar.CookieJar()
    handlers = [urllib.request.HTTPCookieProcessor(jar)] + ([StayOnBase(issuer, base)] if issuer else [])
    browser = urllib.request.build_opener(*handlers)

    def call(method, path, body=None):
        req = urllib.request.Request(base + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                                     headers={"Content-Type": "application/json"} if body is not None else {})
        try:
            with browser.open(req, timeout=10) as r:
                return r.status, json.load(r)
        except urllib.error.HTTPError as e:
            return e.code, json.load(e)

    # /table/ -> /table/signin -> /authorize -> the sign-in page; click the `idp` button;
    # -> /table/signin/callback (sets the cookies) -> /table/, the page.
    with browser.open(base + "/table/", timeout=10) as r:
        request_id = parse_qs(urlparse(r.url).query)["request"][0]
    form = urlencode({"request": request_id}).encode()
    with browser.open(urllib.request.Request(f"{base}/login/{idp}", data=form, method="POST"), timeout=10) as r:
        page = r.read().decode()
        assert urlparse(r.url).path == "/table/" and re.search(r'<base href="/table/">', page), r.url
    print(f"table    {idp} sign-in at /table/: cookies {sorted(c.name for c in jar)}, page {len(page)} bytes")
    status, info = call("GET", "/table/api/info")
    assert status == 200 and info["league"]["account"]["name"], info
    status, s = call("GET", "/table/api/state")
    assert status == 200 and s["session"] and "presence" in s, (status, s)
    status, s = call("POST", "/table/api/new", {"seat": "alternate", "opponent": "turtle"})
    assert status == 200 and s["hand"] == 1, (status, s)
    while s["over"]:
        status, s = call("POST", "/table/api/next", {"hand": s["hand"]})
        assert status == 200, (status, s)
    a = next((x for x in s["actions"] if x["kind"] in ("check", "call")), s["actions"][0])
    status, s2 = call("POST", "/table/api/act", {"hand": s["hand"], "step": s["step"], "action": a["index"]})
    assert status == 200 and s2["presence"]["last"]["name"] == "you", (status, s2)
    print(f"table    api/state + api/new (reset) + api/act({a['kind']}): session {s['session'][:8]}... hand {s2['hand']} step {s['step']} -> {s2['step']}")
    # A program token: made by the signed-in page, it plays the same table, then is revoked.
    status, made = call("POST", "/table/api/token/regenerate", {"label": "smoke"})
    assert status == 200 and made["token"].startswith("obgl_pt_"), (status, made)
    prog = urllib.request.Request(base + "/table/api/state", headers={"Authorization": f"Bearer {made['token']}"})
    with urllib.request.urlopen(prog, timeout=10) as r:
        ps = json.load(r)
    assert ps["session"] == s2["session"] and ps["step"] == s2["step"], ps
    status, _ = call("POST", "/table/api/token/revoke", {})
    assert status == 200, status
    try:
        urllib.request.urlopen(prog, timeout=10)
        raise AssertionError("a revoked program token still reads the table")
    except urllib.error.HTTPError as e:
        assert e.code == 401, e.code
    status, _, _ = get(f"{base}/table/api/state")
    assert status == 401, status
    return True, (f"program token {made['id']} read the same table (hand {ps['hand']} step {ps['step']}), then revoked: 401; "
                  "no cookie: 401")

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--repo", required=True, help="checkout the server was built from")
    ap.add_argument("--sha")
    ap.add_argument("--require", default="")
    ap.add_argument("--sign-in", metavar="IDP", help="sign in with this IdP's button (only a server wired to a test IdP allows it)")
    ap.add_argument("--stay-on-base", action="store_true",
                    help="the table check follows redirects to the OAuth issuer on --url's origin")
    a = ap.parse_args()
    base, required, failed = a.url.rstrip("/"), set(filter(None, a.require.split(","))), False

    def check(name, ok, detail):
        nonlocal failed
        print(f"{name:8} {'ok' if ok else 'FAIL'}: {detail}" if ok is not None else f"{name:8} absent (404): {detail}")
        if ok is False or (ok is None and name in required):
            failed = True

    prm = protected_resource(base)
    token = None
    if prm is None:
        check("oauth", None, "GET /.well-known/oauth-protected-resource/mcp")
    else:
        sign_in_page_check(base, a.repo, prm)
        if a.sign_in:
            token = sign_in_token(base, a.repo, prm, a.sign_in)
    if prm is None or token is not None:
        proto, server, sid = raw_initialize(base, token)
        print(f"mcp      initialize: protocol {proto}, server {server!r}, Mcp-Session-Id {sid}"
              f"{', with the bearer token' if token else ''}")
        asyncio.run(play(base, token))
    else:
        print("mcp      skipped: needs a signed-in account (--sign-in against a server wired to a test IdP)")

    if a.sha:
        status, _, body = get(base + "/version")
        if status == 404:
            check("version", None, "GET /version")
        else:
            got = json.loads(body).get("sha") if status == 200 else None
            check("version", got == a.sha, f"GET /version -> {status} {body.decode().strip()}")

    # The browser table signs in through the same OAuth server: none without it.
    issuer = (prm.get("authorization_servers") or [prm["resource"]])[0] if a.stay_on_base and prm else None
    info = table_info(base)
    if info is None:
        check("table", None, "GET /table/api/info")
    else:
        check("table", *info)
        if token is not None:
            check("table", *table(base, a.sign_in, issuer))
        elif prm is not None:
            print("table    skipped: the signed-in steps need a signed-in account (--sign-in against a server wired to a test IdP)")

    status, hdrs, body = get(base + "/docs/AGENTS.md")
    if status == 404:
        check("docs", None, "GET /docs/AGENTS.md")
    else:
        want = open(os.path.join(a.repo, "docs", "AGENTS.md"), "rb").read()
        check("docs", status == 200 and body == want,
              f"GET /docs/AGENTS.md -> {status} {hdrs.get('content-type')}, {len(body)} bytes, "
              f"{'identical to' if body == want else 'DIFFERENT from'} {a.repo}/docs/AGENTS.md ({len(want)} bytes)")
    for page in ("/docs/privacy", "/docs/terms"):
        status, hdrs, body = get(base + page)
        if status == 404:
            check("docs", None, f"GET {page}")
        else:
            ctype = hdrs.get("content-type") or ""
            check("docs", status == 200 and ctype.startswith("text/html"),
                  f"GET {page} -> {status} {ctype}, {len(body)} bytes")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
