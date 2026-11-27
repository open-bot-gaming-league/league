"""Submitted bots' files in Cloud Storage (bot_files.py has the interface
and the layout): one bucket, the keys as object names. Design and setup:
deploy/cloudrun/BOT-STORAGE.md.

The server starts each upload session itself, with the server's own
credentials and the file's declared size (X-Upload-Content-Length), and
hands the client the session's address: no signed URLs, so no key to sign
with. Reading back (finalize) streams the object; promoting is a
server-side rewrite, then a delete.
"""

from __future__ import annotations

from collections.abc import Iterator

from google.api_core.exceptions import NotFound

from bot_files import CHUNK


class GcsFiles:
    def __init__(self, bucket: str, client=None) -> None:
        if client is None:
            from google.cloud import storage

            client = storage.Client()
        self.bucket = client.bucket(bucket)

    def start_upload(self, key: str, size: int) -> str:
        # checksum=None: the library computes no hash for an upload it does
        # not make (the client's bytes are checked at finalize, by sha256).
        return self.bucket.blob(key).create_resumable_upload_session(
            content_type="application/octet-stream", size=size, checksum=None)

    def size(self, key: str) -> int | None:
        blob = self.bucket.get_blob(key)
        return None if blob is None else blob.size

    def read(self, key: str) -> Iterator[bytes]:
        with self.bucket.blob(key).open("rb", chunk_size=CHUNK) as f:
            while piece := f.read(CHUNK):
                yield piece

    def promote(self, src: str, dst: str) -> None:
        source, target = self.bucket.blob(src), self.bucket.blob(dst)
        token, _, _ = target.rewrite(source)
        while token is not None:  # a big object can take more than one call
            token, _, _ = target.rewrite(source, token=token)
        source.delete()

    def delete(self, key: str) -> None:
        # An unfinished upload session to the key is not cancelled: it ends
        # by itself within a week, and anything it finishes lands under
        # uploads/, which the bucket's lifecycle rule clears.
        try:
            self.bucket.blob(key).delete()
        except NotFound:
            pass
