"""Where submitted bots' files live, behind one small interface: the bytes
of every bot's wasm module and data files. The records about them (who
submitted what, which versions exist) are bots.py's, in the auth store.

    uploads/<account>/<submission>/<sha256>   a file as uploaded, not yet checked
    bots/<account>/<sha256>                   a file checked against its sha256: what bots run

A file is uploaded to its upload key and moved to its bot key only once
the server has read it back and its size and sha256 are what was
declared, so nothing under bots/ is unchecked. Content-addressed per
account: a coach's next version reuses the data the last one uploaded,
and no account can name another's files. Uploads are kept apart under
one prefix so a bucket lifecycle rule can drop abandoned ones.

Uploads never pass through the server's request handling in production:
a 16 GB data file cannot (Cloud Run caps an HTTP/1 request at 32 MiB, see
deploy/cloudrun/BOT-STORAGE.md). The server starts an upload session for
each file, of the file's declared size, and the client sends the bytes
straight to it by Cloud Storage's resumable upload protocol:

    PUT <session>, Content-Range: bytes A-B/TOTAL, body  -> 308 (Range: bytes=0-N) | 200 when whole
    PUT <session>, Content-Range: bytes */TOTAL, no body -> the same answers: where it stands

A session's address is its own permission (as Cloud Storage's are), for
the one file, for a week. LocalFiles (tests, the local stack) is a
directory and serves the protocol itself at /bots/upload/; GcsFiles
(bot_files_gcs.py) starts Cloud Storage's own sessions. One client (the
SDK's `bot submit`) works against both.

The interface is synchronous; bots.py calls it in a worker thread.
"""

from __future__ import annotations

import json
import os
import re
import secrets
import tempfile
import threading
from collections.abc import Iterator
from typing import Protocol

import anyio.to_thread
from starlette.requests import ClientDisconnect, Request
from starlette.responses import JSONResponse, Response

UPLOAD_PATH = "/bots/upload/"
CHUNK = 8 << 20  # bytes read at a time
MAX_CHUNK = 64 << 20  # the most one PUT to a local session may carry
_ACCOUNT = re.compile(r"[A-Za-z0-9_-]+:[A-Za-z0-9_.@-]+")
_HEX = re.compile(r"[0-9a-f]+")


def _part(kind: str, value: str, pattern: re.Pattern) -> str:
    if not pattern.fullmatch(value):
        raise ValueError(f"{kind} {value!r} cannot be part of a file key")
    return value


def upload_key(account: str, submission: str, sha256: str) -> str:
    return f"uploads/{_part('account', account, _ACCOUNT)}/{_part('submission', submission, _HEX)}/{_part('sha256', sha256, _HEX)}"


def bot_key(account: str, sha256: str) -> str:
    return f"bots/{_part('account', account, _ACCOUNT)}/{_part('sha256', sha256, _HEX)}"


class BotFiles(Protocol):
    def start_upload(self, key: str, size: int) -> str:
        """A new upload session for the file at `key`, `size` bytes: the
        address the client PUTs it to. It takes no more than `size` bytes,
        and lasts a week (or until the file is whole)."""
        ...

    def size(self, key: str) -> int | None:
        """The size of the file at `key` once it is whole; None if there is none."""
        ...

    def read(self, key: str) -> Iterator[bytes]:
        """The file's bytes, a piece at a time."""
        ...

    def promote(self, src: str, dst: str) -> None:
        """Move the file at src to dst (replacing dst)."""
        ...

    def delete(self, key: str) -> None:
        """Remove the file at key, and any upload in progress to it; no error if there is none."""
        ...


class LocalFiles:
    """Files in a directory (tests, the local stack), and the upload
    protocol above served at <public_url>/bots/upload/<session id>. A
    session is a partial file beside the store, and never grows past the
    size it was started for."""

    def __init__(self, root: str, public_url: str) -> None:
        self.root = os.path.abspath(root)
        self.public_url = public_url.rstrip("/")
        os.makedirs(os.path.join(self.root, ".sessions"), exist_ok=True)
        self._lock = threading.Lock()  # one session write (or end) at a time

    def _path(self, key: str) -> str:
        path = os.path.normpath(os.path.join(self.root, key))
        if not path.startswith(self.root + os.sep) or os.path.basename(path).startswith("."):
            raise ValueError(f"bad key {key!r}")
        return path

    # ---- the store ------------------------------------------------------------

    def start_upload(self, key: str, size: int) -> str:
        self._path(key)
        upload_id = secrets.token_hex(16)
        meta, part = self._session_files(upload_id)
        with open(meta, "w") as f:
            json.dump({"k": key, "n": size}, f)
        open(part, "wb").close()
        return f"{self.public_url}{UPLOAD_PATH}{upload_id}"

    def size(self, key: str) -> int | None:
        try:
            return os.stat(self._path(key)).st_size
        except FileNotFoundError:
            return None

    def read(self, key: str) -> Iterator[bytes]:
        with open(self._path(key), "rb") as f:
            while piece := f.read(CHUNK):
                yield piece

    def promote(self, src: str, dst: str) -> None:
        target = self._path(dst)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        os.replace(self._path(src), target)
        self._prune(os.path.dirname(self._path(src)))

    def delete(self, key: str) -> None:
        path = self._path(key)
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
        with self._lock:
            for name in os.listdir(os.path.join(self.root, ".sessions")):
                upload_id = name.split(".")[0]
                meta = self._session_meta(upload_id)
                if meta is not None and meta["k"] == key:
                    self._end_session(upload_id)
        self._prune(os.path.dirname(path))

    def _prune(self, directory: str) -> None:
        """Remove empty directories from `directory` up to the root."""
        while directory.startswith(self.root + os.sep):
            try:
                os.rmdir(directory)
            except OSError:
                return
            directory = os.path.dirname(directory)

    # ---- the upload protocol ------------------------------------------------------

    def _session_files(self, upload_id: str) -> tuple[str, str]:
        if not _HEX.fullmatch(upload_id):
            raise ValueError("bad upload id")
        base = os.path.join(self.root, ".sessions", upload_id)
        return base + ".json", base + ".part"

    def _session_meta(self, upload_id: str) -> dict | None:
        try:
            with open(self._session_files(upload_id)[0]) as f:
                return json.load(f)
        except (FileNotFoundError, ValueError):
            return None

    def _end_session(self, upload_id: str) -> None:
        for p in self._session_files(upload_id):
            try:
                os.unlink(p)
            except FileNotFoundError:
                pass

    def put(self, upload_id: str, content_range: str, body: Iterator[bytes] | None) -> tuple[int, dict, str]:
        """A PUT to a session: (status, headers, error). `body` is None for
        a status query, else the chunk's bytes. Bytes the session already
        has are skipped; a chunk that starts past them is refused."""
        if not _HEX.fullmatch(upload_id):
            return 404, {}, "no such upload session"
        with self._lock:
            return self._put(upload_id, content_range, body)

    def _put(self, upload_id: str, content_range: str, body: Iterator[bytes] | None) -> tuple[int, dict, str]:
        meta = self._session_meta(upload_id)
        if meta is None:
            return 404, {}, "no such upload session (it finished, or was abandoned)"
        total = meta["n"]
        _, part = self._session_files(upload_id)
        have = os.path.getsize(part)
        m = re.fullmatch(r"bytes (?:(\d+)-(\d+)|\*)/(\d+)", content_range.strip())
        if not m or int(m.group(3)) != total:
            return 400, {}, f"Content-Range must be bytes A-B/{total} or bytes */{total}"
        if m.group(1) is not None:
            first, last = int(m.group(1)), int(m.group(2))
            if last < first or last >= total:
                return 400, {}, "a Content-Range outside the file"
            if first > have:
                return 400, {}, f"this chunk starts at {first}, but the upload has {have} bytes: resume from there"
            skip, room = have - first, last + 1 - have
            with open(part, "ab") as f:
                for piece in body or ():
                    if skip:
                        cut = min(skip, len(piece))
                        piece, skip = piece[cut:], skip - cut
                    if len(piece) > room:
                        return 400, {}, "more bytes than the Content-Range says"
                    f.write(piece)
                    room -= len(piece)
            have = os.path.getsize(part)
        if have < total:
            return 308, ({"Range": f"bytes=0-{have - 1}"} if have else {}), ""
        target = self._path(meta["k"])
        os.makedirs(os.path.dirname(target), exist_ok=True)
        os.replace(part, target)
        self._end_session(upload_id)
        return 200, {}, ""


def register_upload_routes(server, files: LocalFiles) -> None:
    """The upload protocol, for a LocalFiles store. No program token: the
    session's address is the permission, as a Cloud Storage session's is."""

    async def upload(request: Request) -> Response:
        upload_id = request.path_params["upload_id"]
        content_range = request.headers.get("content-range", "")
        if "*" in content_range:
            status, headers, error = await anyio.to_thread.run_sync(lambda: files.put(upload_id, content_range, None))
        else:
            # The chunk is spooled first (a few MiB, MAX_CHUNK at most), so
            # the write to the session happens in one thread call.
            # A chunk cut off part way is dropped whole: the client asks
            # where the session stands and sends it again.
            with tempfile.SpooledTemporaryFile(max_size=CHUNK) as spool:
                try:
                    async for piece in request.stream():
                        spool.write(piece)
                        if spool.tell() > MAX_CHUNK:
                            return JSONResponse({"error": f"a chunk may be at most {MAX_CHUNK} bytes"}, status_code=413)
                except ClientDisconnect:
                    return Response(status_code=400)  # no one is there to read it
                spool.seek(0)

                def pieces() -> Iterator[bytes]:
                    while piece := spool.read(1 << 20):
                        yield piece

                status, headers, error = await anyio.to_thread.run_sync(lambda: files.put(upload_id, content_range, pieces()))
        if error:
            return JSONResponse({"error": error}, status_code=status, headers=headers)
        return Response(status_code=status, headers=headers)

    server.custom_route(UPLOAD_PATH + "{upload_id}", methods=["PUT"])(upload)
