"""The OAuth authorization server behind the MCP connector.

`LeagueOAuthProvider` implements the mcp SDK's
`OAuthAuthorizationServerProvider` (mcp/server/auth/provider.py in the
installed package); the SDK's own routes (mcp/server/auth/routes.py) do
the protocol work around it: metadata, dynamic client registration,
/authorize request validation, /token (including the PKCE check and code
expiry), /revoke. What is ours:

- `authorize()` does not decide anything. It files the validated request
  as a *pending sign-in* and sends the browser to our sign-in page
  (`/login?request=<id>`, oauth/web.py), which offers GitHub, Google
  (each only if configured) and "Continue as guest".
- The sign-in page's choice lands in `finish_guest()` or, via the
  upstream IdP's redirect, `finish_upstream()`. Either one picks the
  account, mints our authorization code bound to it, and returns the
  URL that sends the browser back to the MCP client's redirect_uri.
- Codes, access and refresh tokens are opaque random strings we mint and
  file in the store by their sha256. Access tokens carry the account id
  as `subject`, so the SDK binds each MCP session to the account that
  opened it, and tools see who is calling (server.py `_poker_for`).

Lifetimes: pending sign-in 10 min, code 5 min, access token 1 h,
refresh token 30 days, rotated (both tokens) on every refresh.
"""

from __future__ import annotations

import base64
import hashlib
import secrets
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from urllib.parse import urlparse

from mcp.server.auth.provider import (
    AccessToken,
    AuthorizationCode,
    AuthorizationParams,
    AuthorizeError,
    OAuthAuthorizationServerProvider,
    RefreshToken,
    TokenError,
    construct_redirect_uri,
)
from mcp.shared.auth import OAuthClientInformationFull, OAuthToken

from .idp import UpstreamError, UpstreamIdP
from .store import AuthStore

PENDING_TTL = 10 * 60
CODE_TTL = 5 * 60
ACCESS_TTL = 60 * 60
REFRESH_TTL = 30 * 24 * 60 * 60

GUEST = "guest"


def _hash(secret: str) -> str:
    return hashlib.sha256(secret.encode()).hexdigest()


def _same_resource(a: str, b: str) -> bool:
    return a.rstrip("/") == b.rstrip("/")


def pkce_pair() -> tuple[str, str]:
    """(code_verifier, S256 code_challenge), RFC 7636."""
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    return verifier, challenge


@dataclass(frozen=True)
class SignInRequest:
    """What the sign-in page shows about a pending request."""

    request_id: str
    client_name: str | None
    redirect_host: str


class SignInError(Exception):
    """The pending sign-in is unknown, expired, already used, or not
    waiting on this IdP. The page says so and sends the user back to
    their client to start again."""


class LeagueOAuthProvider(OAuthAuthorizationServerProvider[AuthorizationCode, RefreshToken, AccessToken]):
    """The SDK protocol (subclassed explicitly, so its default refusal of
    the SEP-990 identity-assertion grant, which we do not offer, applies)
    plus the sign-in steps oauth/web.py calls."""

    def __init__(
        self,
        store: AuthStore,
        *,
        issuer_url: str,
        resource_url: str,
        idps: Sequence[UpstreamIdP] = (),
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.store = store
        self.issuer_url = issuer_url.rstrip("/")
        self.resource_url = resource_url
        self.idps = {idp.name: idp for idp in idps}
        self.clock = clock

    # ---- clients (dynamic client registration) ---------------------------

    async def get_client(self, client_id: str) -> OAuthClientInformationFull | None:
        record = await self.store.get("clients", client_id)
        return OAuthClientInformationFull.model_validate(record) if record else None

    async def register_client(self, client_info: OAuthClientInformationFull) -> None:
        await self.store.put("clients", client_info.client_id, client_info.model_dump(mode="json", exclude_none=True))

    # ---- /authorize -> sign-in page --------------------------------------

    async def authorize(self, client: OAuthClientInformationFull, params: AuthorizationParams) -> str:
        # RFC 8707: the only resource we issue tokens for is our own MCP
        # endpoint. A client that names none still gets a token bound to it.
        if params.resource is not None and not _same_resource(params.resource, self.resource_url):
            raise AuthorizeError("invalid_target", f"this server only issues tokens for {self.resource_url}")
        if params.scopes is not None:
            scopes = params.scopes
        else:
            scopes = client.scope.split() if client.scope else []
        request_id = secrets.token_urlsafe(32)
        await self.store.put(
            "pending",
            request_id,
            {
                "client_id": client.client_id,
                "scopes": scopes,
                "state": params.state,
                "code_challenge": params.code_challenge,
                "redirect_uri": str(params.redirect_uri),
                "redirect_uri_provided_explicitly": params.redirect_uri_provided_explicitly,
                "expires_at": self.clock() + PENDING_TTL,
                "upstream": None,
            },
        )
        return f"{self.issuer_url}/login?request={request_id}"

    async def _pending(self, request_id: str, *, take: bool) -> dict:
        record = await (self.store.take if take else self.store.get)("pending", request_id)
        if record is None:
            raise SignInError("unknown or already used sign-in request")
        if record["expires_at"] < self.clock():
            await self.store.delete("pending", request_id)
            raise SignInError("sign-in request expired")
        return record

    async def sign_in_request(self, request_id: str) -> SignInRequest:
        record = await self._pending(request_id, take=False)
        client = await self.get_client(record["client_id"])
        return SignInRequest(
            request_id=request_id,
            client_name=client.client_name if client else None,
            redirect_host=urlparse(record["redirect_uri"]).netloc,
        )

    async def start_upstream(self, request_id: str, idp_name: str) -> str:
        """The URL that sends the browser to sign in at `idp_name`. The
        pending request id doubles as the upstream `state`."""
        idp = self.idps.get(idp_name)
        if idp is None:
            raise SignInError(f"sign-in with {idp_name!r} is not configured")
        record = await self._pending(request_id, take=False)
        verifier, challenge = pkce_pair()
        record["upstream"] = {"idp": idp_name, "code_verifier": verifier}
        await self.store.put("pending", request_id, record)
        return idp.authorization_url(state=request_id, code_challenge=challenge)

    async def finish_upstream(
        self, idp_name: str, *, state: str, code: str | None, error: str | None = None
    ) -> str:
        """The upstream IdP sent the browser back. Returns the URL that
        sends it on to the MCP client: with our code, or with an error."""
        record = await self._pending(state, take=True)
        upstream = record.get("upstream") or {}
        if upstream.get("idp") != idp_name:
            raise SignInError(f"this sign-in was not started with {idp_name!r}")
        if error or not code:
            # The user said no upstream (error=access_denied), or the IdP failed.
            return self._error_redirect(record, "access_denied" if error == "access_denied" else "server_error")
        try:
            identity = await self.idps[idp_name].identify(code=code, code_verifier=upstream["code_verifier"])
        except UpstreamError:
            return self._error_redirect(record, "server_error", f"sign-in with {idp_name} failed")
        account_id = await self._upsert_account(idp_name, identity.provider_user_id, identity.display_name)
        return await self._issue_code(record, account_id)

    async def finish_guest(self, request_id: str) -> str:
        record = await self._pending(request_id, take=True)
        account_id = await self._upsert_account(GUEST, secrets.token_urlsafe(16), "Guest")
        return await self._issue_code(record, account_id)

    async def cancel(self, request_id: str) -> str:
        record = await self._pending(request_id, take=True)
        return self._error_redirect(record, "access_denied", "the user cancelled sign-in")

    def _error_redirect(self, record: dict, error: str, description: str | None = None) -> str:
        return construct_redirect_uri(
            record["redirect_uri"], error=error, error_description=description, state=record["state"]
        )

    # ---- accounts ----------------------------------------------------------

    async def _upsert_account(self, provider: str, provider_user_id: str, display_name: str) -> str:
        """Account = provider + provider user id + display name, nothing
        else. The display name is refreshed at every sign-in."""
        account_id = f"{provider}:{provider_user_id}"
        await self.store.put(
            "accounts",
            account_id,
            {"provider": provider, "provider_user_id": provider_user_id, "display_name": display_name},
        )
        return account_id

    async def account(self, account_id: str) -> dict | None:
        return await self.store.get("accounts", account_id)

    # ---- table-session ownership ---------------------------------------------
    # Every table session opened through the connector records the account
    # that opened it, so table-side features (owner-only watch links,
    # handing a browser session to the owner's agent) can check a signed-in
    # user against it. The Go table itself stays identity-free.

    async def record_table_session(self, table_session_id: str, account_id: str) -> None:
        await self.store.put(
            "table-sessions", table_session_id, {"owner": account_id, "created_at": int(self.clock())}
        )

    async def table_session_for(self, table_session_id: str, account_id: str) -> dict | None:
        """The ownership record, only if `account_id` owns the session.
        There is deliberately no owner-blind read: another account's
        session looks the same as a session that does not exist."""
        record = await self.store.get("table-sessions", table_session_id)
        if record is None or record["owner"] != account_id:
            return None
        return record

    # ---- codes and tokens --------------------------------------------------

    async def _issue_code(self, record: dict, account_id: str) -> str:
        code = secrets.token_urlsafe(32)
        await self.store.put(
            "codes",
            _hash(code),
            {
                "client_id": record["client_id"],
                "scopes": record["scopes"],
                "expires_at": self.clock() + CODE_TTL,
                "code_challenge": record["code_challenge"],
                "redirect_uri": record["redirect_uri"],
                "redirect_uri_provided_explicitly": record["redirect_uri_provided_explicitly"],
                "resource": self.resource_url,
                "subject": account_id,
            },
        )
        return construct_redirect_uri(record["redirect_uri"], code=code, state=record["state"])

    async def load_authorization_code(
        self, client: OAuthClientInformationFull, authorization_code: str
    ) -> AuthorizationCode | None:
        record = await self.store.get("codes", _hash(authorization_code))
        if record is None or record["client_id"] != client.client_id:
            return None
        return AuthorizationCode(code=authorization_code, **record)

    async def exchange_authorization_code(
        self, client: OAuthClientInformationFull, authorization_code: AuthorizationCode
    ) -> OAuthToken:
        # Single use: whichever of two concurrent redemptions takes it first wins.
        if await self.store.take("codes", _hash(authorization_code.code)) is None:
            raise TokenError("invalid_grant", "authorization code already used")
        return await self._issue_tokens(
            client.client_id, authorization_code.scopes, authorization_code.subject, authorization_code.resource
        )

    async def _issue_tokens(
        self, client_id: str, scopes: list[str], subject: str | None, resource: str | None
    ) -> OAuthToken:
        now = self.clock()
        access = "obgl_at_" + secrets.token_urlsafe(32)
        refresh = "obgl_rt_" + secrets.token_urlsafe(32)
        common = {"client_id": client_id, "scopes": scopes, "subject": subject, "resource": resource}
        await self.store.put(
            "access", _hash(access), {**common, "expires_at": int(now + ACCESS_TTL), "refresh_key": _hash(refresh)}
        )
        await self.store.put(
            "refresh", _hash(refresh), {**common, "expires_at": int(now + REFRESH_TTL), "access_key": _hash(access)}
        )
        return OAuthToken(
            access_token=access,
            expires_in=ACCESS_TTL,
            scope=" ".join(scopes) or None,
            refresh_token=refresh,
        )

    async def load_refresh_token(self, client: OAuthClientInformationFull, refresh_token: str) -> RefreshToken | None:
        record = await self.store.get("refresh", _hash(refresh_token))
        if record is None or record["client_id"] != client.client_id:
            return None
        return RefreshToken(
            token=refresh_token,
            client_id=record["client_id"],
            scopes=record["scopes"],
            expires_at=record["expires_at"],
            resource=record["resource"],
            subject=record["subject"],
        )

    async def exchange_refresh_token(
        self, client: OAuthClientInformationFull, refresh_token: RefreshToken, scopes: list[str]
    ) -> OAuthToken:
        record = await self.store.take("refresh", _hash(refresh_token.token))
        if record is None:
            raise TokenError("invalid_grant", "refresh token already used")
        await self.store.delete("access", record["access_key"])
        return await self._issue_tokens(client.client_id, scopes, record["subject"], record["resource"])

    async def load_access_token(self, token: str) -> AccessToken | None:
        key = _hash(token)
        record = await self.store.get("access", key)
        if record is None:
            return None
        if record["expires_at"] < self.clock():
            await self.store.delete("access", key)
            return None
        return AccessToken(
            token=token,
            client_id=record["client_id"],
            scopes=record["scopes"],
            expires_at=record["expires_at"],
            resource=record["resource"],
            subject=record["subject"],
            claims={"iss": self.issuer_url},
        )

    async def revoke_token(self, token: AccessToken | RefreshToken) -> None:
        """Revokes the pair, whichever half is presented."""
        if isinstance(token, AccessToken):
            record = await self.store.take("access", _hash(token.token))
            if record:
                await self.store.delete("refresh", record["refresh_key"])
        else:
            record = await self.store.take("refresh", _hash(token.token))
            if record:
                await self.store.delete("access", record["access_key"])
