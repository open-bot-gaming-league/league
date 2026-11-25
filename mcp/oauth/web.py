"""The sign-in page and the routes behind it (plain web routes next to
/mcp, outside the bearer-token check; mcp/server.py registers them).

    GET  /login?request=<id>        the page: who is asking, and the choices
    POST /login/cancel              Cancel (back to the client with access_denied)
    POST /login/<idp>               go sign in at GitHub / Google
    GET  /login/<idp>/callback      the IdP's redirect back to us

Every choice is a POST from the page, so a link preview or prefetch
cannot complete a sign-in. There is no cookie and no "remember me": every
/authorize shows the page and needs a click, which is the consent step
the MCP security guidance asks of a server with open client registration
(the page names the client and where it will be sent back to).
"""

from __future__ import annotations

import html
import logging

from mcp.server.mcpserver import MCPServer
from starlette.requests import Request
from starlette.responses import HTMLResponse, RedirectResponse, Response

from pages import legal_link

from .idp import callback_path
from .provider import LeagueOAuthProvider, SignInError

logger = logging.getLogger(__name__)

# A line about what an account stores, rendered under the sign-in buttons
# when non-empty.
PRIVACY_NOTE = ""

# The legal links open in a new tab: the sign-in page stays where it is.
NEW_TAB = " target=_blank rel=noopener"

_HEADERS = {
    "Cache-Control": "no-store",
    "X-Frame-Options": "DENY",
    "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'",
    # The page URL carries the request id; do not hand it to GitHub/Google.
    "Referrer-Policy": "no-referrer",
}

# The page is a self-contained document in the league's look (the table's
# felt and dock: dark card, green felt header, cream buttons). It runs
# under the CSP above: inline style and inline SVG only, no script, no
# external fonts, images or requests. Not pages.render(): that shell is the
# docs' plain reading column.
_DOC = """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{title}</title>
<style>{style}</style>
</head><body>
<main class="card">
{brand}
{body}
</main>
</body></html>
"""

_STYLE = (
    "*{box-sizing:border-box}"
    'body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font:16px/1.5 "Liberation Sans",Arial,Helvetica,sans-serif;padding:1rem;background:#14181b;color:#e7eaed}'
    "form{margin:.55rem 0}"
    "button{font:inherit;width:100%;cursor:pointer;display:flex;align-items:center;justify-content:center;gap:.6rem;padding:.75rem 1rem;border-radius:.6rem}"
    ".mk{display:inline-flex}"
    ".card{width:100%;max-width:25rem;background:#1d2327;border:1px solid #323b42;border-radius:1.1rem;overflow:hidden;padding:0 1.6rem 1.3rem;box-shadow:0 20px 60px #0009}"
    ".brand{display:flex;flex-direction:column;align-items:center;gap:.15rem;text-align:center;margin:0 -1.6rem 1.1rem;padding:1.3rem 1rem 1rem;background:radial-gradient(ellipse at 50% 0%,#2c5c45,#1f4632);box-shadow:inset 0 -.3rem 0 #14291e}"
    ".wm{font-weight:800;letter-spacing:.22em;font-size:1.05rem}"
    ".sub{font-size:.78rem;letter-spacing:.06em;color:#cfe3d6}"
    "h1{font-size:1.35rem;margin:.4rem 0 .5rem;text-align:center}"
    ".who{color:#c5ccd2;text-align:center;font-size:.95rem}.who strong{color:#fff}"
    ".idp{background:#f4f1e8;color:#16191c;border:1px solid #f4f1e8;font-weight:700}.idp:hover{background:#fff}"
    ".cancel{background:none;color:#9aa5ad;border:1px solid #3d464d}"
    ".small{font-size:.84rem;color:#9aa5ad;text-align:center}"
    "a{color:#e9b949}"
    "button:focus-visible,a:focus-visible{outline:3px solid #e9b949;outline-offset:2px}"
)

# The league's mark: two playing cards (an ace and a king) and the name, inline SVG.
_BRAND = (
    '<div class="brand"><svg viewBox="0 0 120 70" width="120" height="70" aria-hidden="true">'
    '<g transform="rotate(-10 45 60)"><rect x="28" y="8" width="38" height="54" rx="4" fill="#fbfaf6" stroke="#0003"/>'
    '<text x="34" y="24" font-size="13" font-weight="700" font-family="Arial" fill="#c62f2a">A</text>'
    '<text x="47" y="48" font-size="20" font-family="Arial" fill="#c62f2a">&#9829;</text></g>'
    '<g transform="rotate(10 75 60)"><rect x="54" y="8" width="38" height="54" rx="4" fill="#fbfaf6" stroke="#0003"/>'
    '<text x="60" y="24" font-size="13" font-weight="700" font-family="Arial" fill="#16191c">K</text>'
    '<text x="72" y="48" font-size="20" font-family="Arial" fill="#16191c">&#9824;</text></g></svg>'
    '<span class="wm">OBGL</span><span class="sub">Open Bot Gaming League</span></div>'
)

# The mark on each sign-in button: a ring around the provider's initial(s),
# not the provider's own logo.
_MARKS = {"github": ("Gh", "8.5"), "google": ("G", "11")}


def _mark(idp_name: str) -> str:
    letters, size = _MARKS.get(idp_name, (idp_name[:1].upper(), "11"))
    return (
        '<span class="mk"><svg viewBox="0 0 20 20" width="20" height="20" aria-hidden="true">'
        '<circle cx="10" cy="10" r="9" fill="none" stroke="currentColor" stroke-width="1.6"/>'
        f'<text x="10" y="14.2" text-anchor="middle" font-size="{size}" font-weight="700" font-family="Arial" '
        f'fill="currentColor">{html.escape(letters)}</text></svg></span>'
    )


def _page(title: str, body: str, status: int = 200) -> HTMLResponse:
    doc = _DOC.format(title=html.escape(title) + " &middot; Open Bot Gaming League", style=_STYLE, brand=_BRAND, body=body)
    return HTMLResponse(doc, status_code=status, headers=_HEADERS)


def _expired(e: SignInError) -> HTMLResponse:
    logger.info("sign-in refused: %s", e)
    return _page(
        "Sign-in link expired",
        "<h1>This sign-in link has expired or was already used</h1>"
        '<p class="who">Go back to Claude and connect again.</p>',
        status=400,
    )


def _redirect(url: str) -> RedirectResponse:
    # 303: the browser follows a POST's answer with a GET.
    return RedirectResponse(url, status_code=303, headers={"Cache-Control": "no-store"})


def _button(action: str, request_id: str, label: str, cls: str, mark: str = "") -> str:
    return (
        f'<form method="post" action="{action}">'
        f'<input type="hidden" name="request" value="{html.escape(request_id)}">'
        f'<button class="{cls}">{mark}{html.escape(label)}</button></form>'
    )


def sign_in_page(provider: LeagueOAuthProvider, request_id: str, client_name: str | None, redirect_host: str) -> str:
    who = html.escape(client_name) if client_name else "An app"
    buttons = [
        _button(f"/login/{idp.name}", request_id, f"Continue with {idp.label}", "idp", _mark(idp.name))
        for idp in provider.idps.values()
    ]
    if not buttons:
        buttons.append('<p class="small">Sign-in is not set up on this server.</p>')
    buttons.append(_button("/login/cancel", request_id, "Cancel", "cancel"))
    privacy = f'<p class="small">{html.escape(PRIVACY_NOTE)}</p>' if PRIVACY_NOTE else ""
    return (
        "<h1>Sign in to play</h1>"
        f'<p class="who"><strong>{who}</strong> wants to play Bot Poker for you. After you choose, '
        f"you go back to <strong>{html.escape(redirect_host)}</strong>.</p>"
        + "".join(buttons)
        + privacy
        + f'<p class="small">By continuing you agree to the {legal_link("terms", NEW_TAB)} and {legal_link("privacy", NEW_TAB)}.</p>'
    )


async def _form_request_id(request: Request) -> str:
    form = await request.form()
    value = form.get("request")
    return value if isinstance(value, str) else ""


def register_login_routes(server: MCPServer, provider: LeagueOAuthProvider) -> None:
    @server.custom_route("/login", methods=["GET"])
    async def login(request: Request) -> Response:
        request_id = request.query_params.get("request", "")
        try:
            req = await provider.sign_in_request(request_id)
        except SignInError as e:
            return _expired(e)
        return _page("Sign in", sign_in_page(provider, req.request_id, req.client_name, req.redirect_host))

    @server.custom_route("/login/cancel", methods=["POST"])
    async def login_cancel(request: Request) -> Response:
        try:
            return _redirect(await provider.cancel(await _form_request_id(request)))
        except SignInError as e:
            return _expired(e)

    def _idp_routes(name: str) -> None:
        @server.custom_route(f"/login/{name}", methods=["POST"])
        async def start(request: Request) -> Response:
            try:
                return _redirect(await provider.start_upstream(await _form_request_id(request), name))
            except SignInError as e:
                return _expired(e)

        @server.custom_route(callback_path(name), methods=["GET"])
        async def callback(request: Request) -> Response:
            q = request.query_params
            try:
                url = await provider.finish_upstream(
                    name, state=q.get("state", ""), code=q.get("code"), error=q.get("error")
                )
            except SignInError as e:
                return _expired(e)
            return _redirect(url)

    for name in provider.idps:
        _idp_routes(name)
