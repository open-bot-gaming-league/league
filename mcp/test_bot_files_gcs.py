"""bot_files_gcs.GcsFiles against a stand-in for the Cloud Storage client
(the calls it makes, recorded): no bucket is touched."""

import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from google.api_core.exceptions import NotFound

from bot_files_gcs import GcsFiles


class FakeBlob:
    def __init__(self, bucket, name):
        self.bucket, self.name = bucket, name

    @property
    def size(self):
        return len(self.bucket.objects[self.name])

    def create_resumable_upload_session(self, content_type=None, size=None, checksum="auto"):
        assert checksum is None
        self.bucket.calls.append(("session", self.name, content_type, size))
        return f"https://storage.googleapis.com/upload/storage/v1/b/{self.bucket.name}/o?upload_id=x"

    def open(self, mode, chunk_size=None):
        assert mode == "rb"
        return io.BytesIO(self.bucket.objects[self.name])

    def rewrite(self, source, token=None):
        self.bucket.calls.append(("rewrite", source.name, self.name, token))
        if token is None:
            return "more", 0, 0  # big objects take a second call
        self.bucket.objects[self.name] = self.bucket.objects[source.name]
        return None, 0, 0

    def delete(self):
        if self.name not in self.bucket.objects:
            raise NotFound("no such object")
        del self.bucket.objects[self.name]


class FakeBucket:
    def __init__(self, name):
        self.name, self.objects, self.calls = name, {}, []

    def blob(self, name):
        return FakeBlob(self, name)

    def get_blob(self, name):
        return FakeBlob(self, name) if name in self.objects else None


class FakeClient:
    def __init__(self):
        self.buckets = {}

    def bucket(self, name):
        return self.buckets.setdefault(name, FakeBucket(name))


def test_gcs_files_map_the_interface_onto_the_bucket():
    client = FakeClient()
    files = GcsFiles("obgl-bots", client)
    bucket = client.buckets["obgl-bots"]
    up, kept = "uploads/github:1/" + "a" * 32 + "/" + "b" * 64, "bots/github:1/" + "b" * 64
    assert files.start_upload(up, 1234).startswith("https://storage.googleapis.com/upload/")
    assert bucket.calls == [("session", up, "application/octet-stream", 1234)]
    assert files.size(up) is None
    bucket.objects[up] = b"x" * (3 << 20)
    assert files.size(up) == 3 << 20
    assert b"".join(files.read(up)) == b"x" * (3 << 20)
    files.promote(up, kept)
    assert bucket.calls[1:] == [("rewrite", up, kept, None), ("rewrite", up, kept, "more")]
    assert list(bucket.objects) == [kept]
    files.delete(kept)
    files.delete(kept)  # gone already: no error
    assert bucket.objects == {}
