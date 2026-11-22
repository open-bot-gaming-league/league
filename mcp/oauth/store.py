"""Where the OAuth server keeps its records, behind one small interface.

Everything the authorization server must remember across requests (and,
with a durable backend, across restarts) is a JSON object filed under a
*kind* and a key:

    clients   registered OAuth clients (dynamic client registration), by client_id
    pending   sign-ins in progress (between /authorize and the user's choice), by request id
    codes     authorization codes, by sha256 of the code
    access    access tokens, by sha256 of the token
    refresh   refresh tokens, by sha256 of the token
    accounts  provider + provider user id + display name, by account id
    table-sessions  who owns a table session (the account that opened it), by table session id

Bearer secrets (codes, tokens) are filed by their sha256, never in the
clear, so a copy of the store is not a copy of anyone's tokens. (Registered
clients' secrets are the exception: the SDK's ClientAuthenticator compares
`client_secret` as stored, so it is kept as issued.)

A record with an "expires_at" field (unix seconds) is the caller's to
expire: the provider checks it on read. Stores may drop expired records
whenever convenient (FileStore does on load).

Backends: MemoryStore (tests; a bare local run), FileStore (one JSON file,
rewritten atomically on every change: survives a restart on the same
disk). `store_from_env` picks one.
"""

from __future__ import annotations

import copy
import json
import os
import tempfile
import time
from collections.abc import Mapping
from typing import Any, Protocol

KINDS = ("clients", "pending", "codes", "access", "refresh", "accounts", "table-sessions")

Record = dict[str, Any]


class AuthStore(Protocol):
    async def get(self, kind: str, key: str) -> Record | None:
        """The record, or None. The caller gets its own copy."""
        ...

    async def put(self, kind: str, key: str, value: Record) -> None:
        """Create or replace."""
        ...

    async def delete(self, kind: str, key: str) -> None:
        """Remove if present; no error if not."""
        ...

    async def take(self, kind: str, key: str) -> Record | None:
        """Get and delete in one step, so a single-use record (an
        authorization code, a refresh token being rotated) can be
        redeemed at most once even under concurrent requests."""
        ...


def _check_kind(kind: str) -> None:
    if kind not in KINDS:
        raise ValueError(f"unknown record kind {kind!r}")


class MemoryStore:
    """In-process dicts. Lost when the process exits. Every method body
    runs without an await, so on one event loop each call is atomic."""

    def __init__(self) -> None:
        self._data: dict[str, dict[str, Record]] = {kind: {} for kind in KINDS}

    async def get(self, kind: str, key: str) -> Record | None:
        _check_kind(kind)
        value = self._data[kind].get(key)
        return copy.deepcopy(value) if value is not None else None

    async def put(self, kind: str, key: str, value: Record) -> None:
        _check_kind(kind)
        self._data[kind][key] = copy.deepcopy(value)
        self._changed()

    async def delete(self, kind: str, key: str) -> None:
        _check_kind(kind)
        if self._data[kind].pop(key, None) is not None:
            self._changed()

    async def take(self, kind: str, key: str) -> Record | None:
        _check_kind(kind)
        value = self._data[kind].pop(key, None)
        if value is not None:
            self._changed()
        return value

    def _changed(self) -> None:
        """Hook for FileStore; nothing to do in memory."""


class FileStore(MemoryStore):
    """MemoryStore plus one JSON file holding all of it, rewritten after
    every change (write to a temp file in the same directory, fsync,
    os.replace): a crash leaves the old file or the new one, never half
    of either. Mode 0600. Fine for one process and the record counts of
    a play-money service; not for several processes sharing one file
    (each would overwrite the others' changes).
    Expired records are dropped when the file is loaded."""

    def __init__(self, path: str) -> None:
        super().__init__()
        self.path = path
        if os.path.exists(path):
            with open(path, encoding="utf-8") as f:
                loaded = json.load(f)
            now = time.time()
            for kind in KINDS:
                for key, value in loaded.get(kind, {}).items():
                    expires_at = value.get("expires_at")
                    if expires_at is None or expires_at >= now:
                        self._data[kind][key] = value
        else:
            os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)

    def _changed(self) -> None:
        directory = os.path.dirname(os.path.abspath(self.path))
        fd, tmp = tempfile.mkstemp(dir=directory, prefix=".oauth-", suffix=".tmp")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(self._data, f, separators=(",", ":"))
                f.flush()
                os.fsync(f.fileno())
            os.chmod(tmp, 0o600)
            os.replace(tmp, self.path)
        except BaseException:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise


def store_from_env(env: Mapping[str, str]) -> AuthStore:
    """OBGL_AUTH_STORE picks the backend:
    - `memory` (the default): lost on restart.
    - `file`: OBGL_AUTH_FILE, else $OBGL_STATE_DIR/oauth.json."""
    kind = env.get("OBGL_AUTH_STORE") or "memory"
    if kind == "memory":
        return MemoryStore()
    if kind == "file":
        path = env.get("OBGL_AUTH_FILE")
        if not path:
            state_dir = env.get("OBGL_STATE_DIR")
            if not state_dir:
                raise ValueError("OBGL_AUTH_STORE=file needs OBGL_AUTH_FILE or OBGL_STATE_DIR")
            path = os.path.join(state_dir, "oauth.json")
        return FileStore(path)
    raise ValueError(f"OBGL_AUTH_STORE={kind!r}: choose memory or file")
