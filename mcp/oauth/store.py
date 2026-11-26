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
    table-sessions  who owns a table session, by table session id
    watch-tokens    a table session's old watch token -> the session, by sha256 of the token
                    (no longer made; kept so account deletion finds the old ones)
    account-tables  an account's table sessions, newest first, by account id
                    (the newest is the account's one table)
    (the last three belong to mcp/tables.py: the browser table)
    program-tokens    a program token -> its account, by sha256 of the token
    account-programs  an account's program tokens (ids, labels; never the tokens), by account id
    program-usage     when a program token was last used, by token id
    (the last three belong to mcp/programs.py)

Bearer secrets (codes, tokens) are filed by their sha256, never in the
clear, so a copy of the store is not a copy of anyone's tokens. (Registered
clients' secrets are the exception: the SDK's ClientAuthenticator compares
`client_secret` as stored, so it is kept as issued.)

A record with an "expires_at" field (unix seconds) is the caller's to
expire: the provider checks it on read. Stores may drop expired records
whenever convenient (FileStore does on load; FirestoreStore by TTL policy,
and it reads an expired record as absent).

Backends: MemoryStore (tests; a bare local run), FileStore (one JSON file,
rewritten atomically on every change: survives a restart on the same
disk, one process only), FirestoreStore (Cloud Run: durable, shared by
every instance). `store_from_env` picks one.
"""

from __future__ import annotations

import asyncio
import copy
import datetime
import json
import os
import tempfile
import time
from collections.abc import Mapping
from typing import Any, Protocol

KINDS = (
    "clients", "pending", "codes", "access", "refresh", "accounts",
    "table-sessions", "watch-tokens", "account-tables",
    "program-tokens", "account-programs", "program-usage",
)

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


class FirestoreStore:
    """Firestore (native mode): the durable store for Cloud Run, shared by
    every instance, so a cold start or a second instance signs no one out.

    Layout: one collection per kind, `oauth_<kind>` (`oauth_codes`,
    `oauth_table-sessions`, ...); document id = the key. Each document has
    two fields:

        record      the record, JSON-encoded (a string), so it reads back
                    exactly as written -- the same round trip FileStore
                    makes, with none of Firestore's own type rules (no
                    nested arrays, ints vs floats) in the way
        expires_at  a Timestamp, present only when the record has an
                    "expires_at": the field a Firestore TTL policy on each
                    collection deletes by (CLOUDRUN.md, OAuth, Storage)

    TTL deletion is lazy (typically within a day of expiry), so reads also
    treat a record past its expires_at as absent.

    `take` reads the document, then deletes it with a precondition: only
    if it is unchanged since that read (its update time). Of concurrent
    takers exactly one delete succeeds; the others' preconditions fail and
    they read again, finding nothing.

    The project comes from the argument (store_from_env passes
    GOOGLE_CLOUD_PROJECT), else Application Default Credentials (on Cloud Run: the service's own
    project). With FIRESTORE_EMULATOR_HOST set, the client talks to that
    emulator instead, without credentials.

    The async client's gRPC channel belongs to the event loop it was made
    on, so the client is made on first use and remade if the loop changes
    (pytest gives each test its own loop; the server has one)."""

    def __init__(self, project: str | None = None) -> None:
        # Imported here, not at the top: the Google client (gRPC and all)
        # loads only in a process that uses Firestore, not in stdio runs
        # or the memory/file-store tests.
        from google.cloud import firestore

        self._firestore = firestore
        self.project = project or _adc_project()
        if not self.project:
            raise ValueError(
                "OBGL_AUTH_STORE=firestore: no Google Cloud project "
                "(set GOOGLE_CLOUD_PROJECT, or run where Application Default Credentials name one)"
            )
        self._client = None
        self._loop = None

    def _db(self):
        loop = asyncio.get_running_loop()
        if self._client is None or self._loop is not loop:
            self._client = self._firestore.AsyncClient(project=self.project)
            self._loop = loop
        return self._client

    def _doc(self, kind: str, key: str):
        _check_kind(kind)
        _check_doc_id(key)
        return self._db().collection(f"oauth_{kind}").document(key)

    @staticmethod
    def _live(snapshot) -> Record | None:
        if not snapshot.exists:
            return None
        value = json.loads(snapshot.get("record"))
        expires_at = value.get("expires_at")
        if expires_at is not None and expires_at < time.time():
            return None
        return value

    async def get(self, kind: str, key: str) -> Record | None:
        return self._live(await self._doc(kind, key).get())

    async def put(self, kind: str, key: str, value: Record) -> None:
        ref = self._doc(kind, key)
        body: dict[str, Any] = {"record": json.dumps(value, separators=(",", ":"))}
        expires_at = value.get("expires_at")
        if expires_at is not None:
            body["expires_at"] = datetime.datetime.fromtimestamp(expires_at, tz=datetime.timezone.utc)
        await ref.set(body)

    async def delete(self, kind: str, key: str) -> None:
        await self._doc(kind, key).delete()

    async def take(self, kind: str, key: str) -> Record | None:
        from google.api_core import exceptions

        ref = self._doc(kind, key)
        client = self._db()
        while True:
            snapshot = await ref.get()
            if not snapshot.exists:
                return None
            try:
                await ref.delete(option=client.write_option(last_update_time=snapshot.update_time))
            except (exceptions.FailedPrecondition, exceptions.Aborted):
                # FailedPrecondition: someone else took (or rewrote) it since
                # our read. Aborted: write contention. Either way, look again.
                continue
            # Ours, and gone. An expired one is gone too, but was never there to take.
            return self._live(snapshot)


def _adc_project() -> str | None:
    import google.auth
    import google.auth.exceptions

    try:
        return google.auth.default()[1]
    except google.auth.exceptions.DefaultCredentialsError:
        return None


def _check_doc_id(key: str) -> None:
    """Firestore's document-id rules, checked here so a bad key fails with
    its name instead of as a path error deep in the client."""
    if (
        not key
        or "/" in key
        or key in (".", "..")
        or (key.startswith("__") and key.endswith("__"))
        or len(key.encode()) > 1500
    ):
        raise ValueError(f"key {key!r} cannot be a Firestore document id")


def store_from_env(env: Mapping[str, str]) -> AuthStore:
    """OBGL_AUTH_STORE picks the backend:
    - `firestore`: the default when OBGL_PUBLIC_URL is set (OAuth on: a
      deployed server). Project: GOOGLE_CLOUD_PROJECT, else ADC.
    - `memory`: the default otherwise; lost on restart.
    - `file`: OBGL_AUTH_FILE, else $OBGL_STATE_DIR/oauth.json."""
    kind = env.get("OBGL_AUTH_STORE") or ("firestore" if env.get("OBGL_PUBLIC_URL") else "memory")
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
    if kind == "firestore":
        return FirestoreStore(env.get("GOOGLE_CLOUD_PROJECT"))
    raise ValueError(f"OBGL_AUTH_STORE={kind!r}: choose memory, file or firestore")
