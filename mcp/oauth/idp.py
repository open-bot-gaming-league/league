"""Upstream identity providers (GitHub, Google): who the person signing in is.

Our server is the OAuth authorization server for the MCP connector, and
it mints its own tokens. An upstream IdP only answers one question, at
sign-in: which GitHub/Google account is this? We run our own, separate
authorization-code flow (with PKCE) against it, using our own OAuth app,
read the account's id and display name, and throw its token away. The
MCP client (claude.ai) never sees the upstream IdP, and the upstream IdP
never sees the MCP client.

`UpstreamIdP` is the interface the sign-in routes use; `OAuth2IdP` is
the one implementation (authorization code + PKCE, then one userinfo
GET), and `github()` / `google()` configure it. Endpoints are
constructor arguments so a test can point them at a fake IdP.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol
from urllib.parse import urlencode, urlparse

import httpx2


@dataclass(frozen=True)
class UpstreamIdentity:
    provider_user_id: str
    display_name: str


class UpstreamError(Exception):
    """The upstream IdP refused or failed the exchange. The message is
    for logs; the user sees a generic failure."""


class UpstreamIdP(Protocol):
    name: str  # route segment and account-id prefix: "github"
    label: str  # button text: "GitHub"

    def authorization_url(self, *, state: str, code_challenge: str) -> str:
        """Where to send the browser to sign in upstream."""
        ...

    async def identify(self, *, code: str, code_verifier: str) -> UpstreamIdentity:
        """Redeem the upstream code and say whose account it was.
        Raises UpstreamError."""
        ...


@dataclass
class OAuth2IdP:
    name: str
    label: str
    client_id: str
    client_secret: str
    redirect_uri: str
    authorize_endpoint: str
    token_endpoint: str
    userinfo_endpoint: str
    scope: str
    identity_from: Callable[[Mapping[str, Any]], UpstreamIdentity]
    extra_authorize_params: Mapping[str, str] = field(default_factory=dict)
    timeout: float = 10.0

    def authorization_url(self, *, state: str, code_challenge: str) -> str:
        params = {
            "response_type": "code",
            "client_id": self.client_id,
            "redirect_uri": self.redirect_uri,
            "state": state,
            "code_challenge": code_challenge,
            "code_challenge_method": "S256",
            **self.extra_authorize_params,
        }
        if self.scope:
            params["scope"] = self.scope
        return f"{self.authorize_endpoint}?{urlencode(params)}"

    async def identify(self, *, code: str, code_verifier: str) -> UpstreamIdentity:
        async with httpx2.AsyncClient(timeout=self.timeout, headers={"User-Agent": "obgl-mcp"}) as http:
            try:
                r = await http.post(
                    self.token_endpoint,
                    data={
                        "grant_type": "authorization_code",
                        "code": code,
                        "redirect_uri": self.redirect_uri,
                        "client_id": self.client_id,
                        "client_secret": self.client_secret,
                        "code_verifier": code_verifier,
                    },
                    headers={"Accept": "application/json"},
                )
                body = r.json()
            except (httpx2.HTTPError, ValueError) as e:
                raise UpstreamError(f"{self.name} token endpoint: {e!r}") from e
            # GitHub answers a bad code with 200 and an "error" field.
            access_token = body.get("access_token") if isinstance(body, dict) else None
            if r.status_code != 200 or not access_token:
                error = body.get("error") if isinstance(body, dict) else None
                raise UpstreamError(f"{self.name} token endpoint: HTTP {r.status_code}, error={error!r}")
            try:
                u = await http.get(
                    self.userinfo_endpoint,
                    headers={"Authorization": f"Bearer {access_token}", "Accept": "application/json"},
                )
                u.raise_for_status()
                info = u.json()
            except (httpx2.HTTPError, ValueError) as e:
                raise UpstreamError(f"{self.name} userinfo: {e!r}") from e
        try:
            return self.identity_from(info)
        except (KeyError, TypeError, ValueError) as e:
            raise UpstreamError(f"{self.name} userinfo: unexpected shape ({e!r})") from e


def _github_identity(info: Mapping[str, Any]) -> UpstreamIdentity:
    # The numeric id is stable; the login can be renamed, so it is only
    # the display name (refreshed at every sign-in). `name` (often a real
    # name) is deliberately not used: the login is what GitHub shows
    # publicly anyway.
    return UpstreamIdentity(provider_user_id=str(int(info["id"])), display_name=str(info["login"]))


def _google_identity(info: Mapping[str, Any]) -> UpstreamIdentity:
    sub = str(info["sub"])
    if not sub:
        raise ValueError("empty sub")
    return UpstreamIdentity(provider_user_id=sub, display_name=str(info.get("name") or "Google user"))


def github(
    client_id: str,
    client_secret: str,
    redirect_uri: str,
    *,
    authorize_endpoint: str = "https://github.com/login/oauth/authorize",
    token_endpoint: str = "https://github.com/login/oauth/access_token",
    userinfo_endpoint: str = "https://api.github.com/user",
) -> OAuth2IdP:
    """A GitHub OAuth app. No scope: an empty scope grants read access to
    public profile data, which is all /user needs."""
    return OAuth2IdP(
        name="github",
        label="GitHub",
        client_id=client_id,
        client_secret=client_secret,
        redirect_uri=redirect_uri,
        authorize_endpoint=authorize_endpoint,
        token_endpoint=token_endpoint,
        userinfo_endpoint=userinfo_endpoint,
        scope="",
        identity_from=_github_identity,
    )


def google(
    client_id: str,
    client_secret: str,
    redirect_uri: str,
    *,
    authorize_endpoint: str = "https://accounts.google.com/o/oauth2/v2/auth",
    token_endpoint: str = "https://oauth2.googleapis.com/token",
    userinfo_endpoint: str = "https://openidconnect.googleapis.com/v1/userinfo",
) -> OAuth2IdP:
    """A Google OAuth client (web application type). Scope `openid
    profile`: the stable `sub` and a display name, no email."""
    return OAuth2IdP(
        name="google",
        label="Google",
        client_id=client_id,
        client_secret=client_secret,
        redirect_uri=redirect_uri,
        authorize_endpoint=authorize_endpoint,
        token_endpoint=token_endpoint,
        userinfo_endpoint=userinfo_endpoint,
        scope="openid profile",
        identity_from=_google_identity,
    )


IDP_FACTORIES = {"github": github, "google": google}


def callback_path(name: str) -> str:
    """The path our server serves an IdP's redirect on (the redirect URI
    registered with that IdP must point here)."""
    return f"/login/{name}/callback"


def idps_from_env(env: Mapping[str, str], public_url: str) -> list[UpstreamIdP]:
    """GitHub and Google, each only if configured:

        OBGL_GITHUB_CLIENT_ID, OBGL_GITHUB_CLIENT_SECRET, OBGL_GITHUB_REDIRECT_URI
        OBGL_GOOGLE_CLIENT_ID, OBGL_GOOGLE_CLIENT_SECRET, OBGL_GOOGLE_REDIRECT_URI

    Neither id nor secret set: that IdP is off (its button is hidden).
    Only one of them set: a startup error, not a silently missing button.
    The redirect URI defaults to <public url>/login/<name>/callback; if
    set, its path must be exactly that (it is the route we serve)."""
    out: list[UpstreamIdP] = []
    for name, factory in IDP_FACTORIES.items():
        prefix = f"OBGL_{name.upper()}_"
        client_id = env.get(prefix + "CLIENT_ID") or ""
        secret = env.get(prefix + "CLIENT_SECRET") or ""
        if not client_id and not secret:
            continue
        if not (client_id and secret):
            raise ValueError(f"{prefix}CLIENT_ID and {prefix}CLIENT_SECRET must both be set, or neither")
        redirect_uri = env.get(prefix + "REDIRECT_URI") or public_url.rstrip("/") + callback_path(name)
        if urlparse(redirect_uri).path != callback_path(name):
            raise ValueError(f"{prefix}REDIRECT_URI must have the path {callback_path(name)}, got {redirect_uri!r}")
        out.append(factory(client_id, secret, redirect_uri))
    return out
