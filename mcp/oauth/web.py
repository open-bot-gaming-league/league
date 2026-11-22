"""The sign-in page and the routes behind it (plain web routes next to
/mcp, outside the bearer-token check; mcp/server.py registers them).

    GET  /login?request=<id>        the page: who is asking, and three choices
    POST /login/guest               Continue as guest
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

from pages import render

from .idp import callback_path
from .provider import LeagueOAuthProvider, SignInError

logger = logging.getLogger(__name__)

# A line about what an account stores, rendered under the sign-in buttons
# when non-empty.
PRIVACY_NOTE = ""

_HEADERS = {
    "Cache-Control": "no-store",
    "X-Frame-Options": "DENY",
    "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'",
    # The page URL carries the request id; do not hand it to GitHub/Google.
    "Referrer-Policy": "no-referrer",
}

_STYLE = (
    "form{margin:.5rem 0}button{font:inherit;width:100%;padding:.6rem;border-radius:.4rem;"
    "border:1px solid #888;background:#f4f4f4;cursor:pointer}button.primary{background:#222;color:#fff}"
    ".small{font-size:.875rem;color:#555}"
)


def _page(title: str, body: str, status: int = 200) -> HTMLResponse:
    return HTMLResponse(render(title, body, _STYLE), status_code=status, headers=_HEADERS)


def _expired(e: SignInError) -> HTMLResponse:
    logger.info("sign-in refused: %s", e)
    return _page(
        "Sign-in link expired",
        "<h1>This sign-in link has expired or was already used</h1>"
        "<p>Go back to Claude and connect again.</p>",
        status=400,
    )


def _redirect(url: str) -> RedirectResponse:
    # 303: the browser follows a POST's answer with a GET.
    return RedirectResponse(url, status_code=303, headers={"Cache-Control": "no-store"})


def _button(action: str, request_id: str, label: str, primary: bool = False) -> str:
    cls = ' class="primary"' if primary else ""
    return (
        f'<form method="post" action="{action}">'
        f'<input type="hidden" name="request" value="{html.escape(request_id)}">'
        f"<button{cls}>{html.escape(label)}</button></form>"
    )


def sign_in_page(provider: LeagueOAuthProvider, request_id: str, client_name: str | None, redirect_host: str) -> str:
    who = html.escape(client_name) if client_name else "An app"
    buttons = [_button(f"/login/{idp.name}", request_id, f"Sign in with {idp.label}", primary=True) for idp in provider.idps.values()]
    buttons.append(_button("/login/guest", request_id, "Continue as guest"))
    buttons.append(_button("/login/cancel", request_id, "Cancel"))
    privacy = f'<p class="small">{html.escape(PRIVACY_NOTE)}</p>' if PRIVACY_NOTE else ""
    return (
        "<h1>Open Bot Gaming League</h1>"
        f"<p><strong>{who}</strong> wants to play Bot Poker for you. After you choose, "
        f"you go back to <strong>{html.escape(redirect_host)}</strong>.</p>"
        + "".join(buttons)
        + '<p class="small">A guest has no account: nothing links your games to you once this connection ends.</p>'
        + privacy
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

    @server.custom_route("/login/guest", methods=["POST"])
    async def login_guest(request: Request) -> Response:
        try:
            return _redirect(await provider.finish_guest(await _form_request_id(request)))
        except SignInError as e:
            return _expired(e)

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
