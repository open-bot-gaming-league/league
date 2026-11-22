"""OAuth for the MCP connector: our server is the authorization server.

Read in this order: provider.py (the SDK provider and the sign-in
steps), web.py (the sign-in page and its routes), idp.py (GitHub and
Google, upstream), store.py (where records live). This file is the
configuration and the one call mcp/server.py makes.

Environment (HTTP transport only):

    OBGL_PUBLIC_URL     the server's public origin, e.g. https://obgl.truemirrorgames.com.
                        Set: OAuth is on, and /mcp needs a bearer token. Unset: no auth
                        (a bare local run; tests without OAuth).
    OBGL_AUTH_STORE     firestore (default) | file | memory -- see store.py
    GOOGLE_CLOUD_PROJECT  Firestore's project (default: the one ADC reports)
    OBGL_AUTH_FILE      the file store's path (default $OBGL_STATE_DIR/oauth.json)
    OBGL_GITHUB_CLIENT_ID / _CLIENT_SECRET / _REDIRECT_URI    see idp.py
    OBGL_GOOGLE_CLIENT_ID / _CLIENT_SECRET / _REDIRECT_URI
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from urllib.parse import urlparse

from mcp.server.auth.settings import AuthSettings, ClientRegistrationOptions, RevocationOptions

from .idp import UpstreamIdP, idps_from_env
from .provider import LeagueOAuthProvider
from .store import AuthStore, MemoryStore, store_from_env
from .web import register_login_routes

__all__ = ["OAuthConfig", "LeagueOAuthProvider", "register_login_routes"]


@dataclass
class OAuthConfig:
    public_url: str  # origin only, no trailing slash: the OAuth issuer
    mcp_path: str  # the MCP endpoint's path; public_url + mcp_path is the token audience
    store: AuthStore = field(default_factory=MemoryStore)
    idps: Sequence[UpstreamIdP] = ()

    def __post_init__(self) -> None:
        self.public_url = self.public_url.rstrip("/")
        u = urlparse(self.public_url)
        if u.scheme not in ("http", "https") or not u.netloc or u.path or u.query or u.fragment:
            raise ValueError(f"OBGL_PUBLIC_URL must be an origin like https://host[:port], got {self.public_url!r}")

    @property
    def resource_url(self) -> str:
        return self.public_url + self.mcp_path

    @classmethod
    def from_env(cls, env: Mapping[str, str], mcp_path: str) -> OAuthConfig | None:
        public_url = env.get("OBGL_PUBLIC_URL")
        if not public_url:
            return None
        return cls(
            public_url=public_url,
            mcp_path=mcp_path,
            store=store_from_env(env),
            idps=idps_from_env(env, public_url),
        )

    def provider(self) -> LeagueOAuthProvider:
        return LeagueOAuthProvider(
            self.store, issuer_url=self.public_url, resource_url=self.resource_url, idps=self.idps
        )

    def auth_settings(self) -> AuthSettings:
        # No scopes: there is one thing a token can do (play), and an
        # advertised scope list is one more thing a client's registration
        # can disagree with. Tokens are bound to our /mcp (RFC 8707) and
        # the SDK refuses any other audience.
        return AuthSettings(
            issuer_url=self.public_url,
            resource_server_url=self.resource_url,
            validate_token_resource=True,
            service_documentation_url=self.public_url + "/docs/",
            client_registration_options=ClientRegistrationOptions(enabled=True),
            revocation_options=RevocationOptions(enabled=True),
        )
