"""One contract, every backend: the AuthStore tests run against
MemoryStore, FileStore and FirestoreStore alike.

FirestoreStore runs against the Firestore emulator, and only when
FIRESTORE_EMULATOR_HOST names one (otherwise those cases skip). Each test
gets its own emulator project, so tests never see each other's records.
Starting the emulator (needs Java and the firebase CLI; no Google account
or project):

    firebase emulators:start --only firestore --project demo-obgl   # prints host:port
    FIRESTORE_EMULATOR_HOST=127.0.0.1:8080 python -m pytest mcp/test_oauth_store.py -q
"""

import asyncio
import datetime
import os
import sys
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import KINDS, FileStore, FirestoreStore, MemoryStore

EMULATOR = os.environ.get("FIRESTORE_EMULATOR_HOST")
FAR = 4_000_000_000  # an expires_at well in the future (2096)


@pytest.fixture(params=["memory", "file", "firestore"])
def open_store(request, tmp_path):
    """A factory: each call opens the same backing store afresh (a new
    process's view: the file reloaded, a new Firestore client). Memory has
    no backing, so it hands back the one instance."""
    if request.param == "memory":
        store = MemoryStore()
        return lambda: store
    if request.param == "file":
        path = str(tmp_path / "oauth.json")
        return lambda: FileStore(path)
    if not EMULATOR:
        pytest.skip("FIRESTORE_EMULATOR_HOST not set (no Firestore emulator)")
    project = f"demo-obgl-{uuid.uuid4().hex[:12]}"
    return lambda: FirestoreStore(project)


@pytest.fixture
def store(open_store):
    return open_store()


@pytest.mark.asyncio
async def test_put_get_replace_delete(store):
    assert await store.get("clients", "c1") is None
    await store.put("clients", "c1", {"client_id": "c1", "n": 1})
    assert await store.get("clients", "c1") == {"client_id": "c1", "n": 1}
    await store.put("clients", "c1", {"client_id": "c1", "n": 2})
    assert await store.get("clients", "c1") == {"client_id": "c1", "n": 2}
    await store.delete("clients", "c1")
    assert await store.get("clients", "c1") is None
    await store.delete("clients", "c1")  # absent: no error


@pytest.mark.asyncio
async def test_records_round_trip_exactly(store):
    record = {
        "s": "\u00e9t\u00e9 \U0001f0a1",
        "i": 2**53,
        "f": 1.5,
        "whole_float": 3.0,
        "b": False,
        "none": None,
        "list": [1, "a", None, [2, [3]]],  # nested arrays: no native Firestore equivalent
        "map": {"": "empty key", "a.b": {"c": []}},
        "expires_at": FAR + 0.25,
    }
    await store.put("codes", "k", record)
    got = await store.get("codes", "k")
    assert got == record
    assert type(got["whole_float"]) is float and type(got["i"]) is int


@pytest.mark.asyncio
async def test_callers_get_copies(store):
    value = {"redirect_uris": ["https://a.example/cb"]}
    await store.put("clients", "c", value)
    value["redirect_uris"].append("https://evil.example/cb")
    got = await store.get("clients", "c")
    assert got == {"redirect_uris": ["https://a.example/cb"]}
    got["redirect_uris"].clear()
    assert await store.get("clients", "c") == {"redirect_uris": ["https://a.example/cb"]}


@pytest.mark.asyncio
async def test_kinds_are_separate(store):
    for kind in KINDS:
        await store.put(kind, "same-key", {"kind": kind})
    for kind in KINDS:
        assert await store.get(kind, "same-key") == {"kind": kind}
    await store.delete("access", "same-key")
    assert await store.get("access", "same-key") is None
    assert await store.get("refresh", "same-key") == {"kind": "refresh"}
    with pytest.raises(ValueError):
        await store.get("tokens", "same-key")


@pytest.mark.asyncio
async def test_take_is_get_and_delete(store):
    await store.put("codes", "once", {"x": 1, "expires_at": FAR})
    assert await store.take("codes", "once") == {"x": 1, "expires_at": FAR}
    assert await store.take("codes", "once") is None
    assert await store.get("codes", "once") is None
    assert await store.take("codes", "never") is None


@pytest.mark.asyncio
async def test_twenty_concurrent_takes_one_winner(open_store):
    """A code redeemed by twenty requests at once is redeemed once. The
    takers are spread over four store instances (four clients: four
    server instances, where the backend has a shared backing)."""
    stores = [open_store() for _ in range(4)]
    for round_ in range(3):
        key = f"code-{round_}"
        await stores[0].put("codes", key, {"round": round_, "expires_at": FAR})
        results = await asyncio.gather(*(stores[i % 4].take("codes", key) for i in range(20)))
        winners = [r for r in results if r is not None]
        assert winners == [{"round": round_, "expires_at": FAR}], results
        assert await stores[1].get("codes", key) is None


@pytest.mark.asyncio
async def test_survives_reopen(open_store):
    await open_store().put("accounts", "github:abc", {"provider": "github"})
    await open_store().put("table-sessions", "t-1", {"owner": "github:abc"})
    reopened = open_store()
    assert await reopened.get("accounts", "github:abc") == {"provider": "github"}
    assert await reopened.get("table-sessions", "t-1") == {"owner": "github:abc"}


# ---- Firestore only ---------------------------------------------------------

firestore_only = pytest.mark.skipif(not EMULATOR, reason="FIRESTORE_EMULATOR_HOST not set (no Firestore emulator)")


@pytest.fixture
def fs():
    return FirestoreStore(f"demo-obgl-{uuid.uuid4().hex[:12]}")


@firestore_only
@pytest.mark.asyncio
async def test_firestore_layout_and_ttl_field(fs):
    """What the deployer's TTL policy relies on: collection oauth_<kind>, document
    id = key, expires_at a Timestamp (absent when the record has none)."""
    await fs.put("access", "h1", {"sub": "github:a", "expires_at": FAR})
    await fs.put("accounts", "github:a", {"provider": "github"})
    db = fs._db()
    doc = (await db.collection("oauth_access").document("h1").get()).to_dict()
    assert set(doc) == {"record", "expires_at"}
    assert isinstance(doc["expires_at"], datetime.datetime)
    assert doc["expires_at"] == datetime.datetime.fromtimestamp(FAR, tz=datetime.timezone.utc)
    assert set((await db.collection("oauth_accounts").document("github:a").get()).to_dict()) == {"record"}


@firestore_only
@pytest.mark.asyncio
async def test_firestore_reads_expired_as_absent(fs):
    """TTL deletion lags (up to a day), so an expired record that is still
    there reads as gone, and a take of it finds nothing (and clears it)."""
    await fs.put("codes", "old", {"expires_at": 1})
    await fs.put("refresh", "old", {"expires_at": 1})
    assert await fs.get("codes", "old") is None
    assert await fs.take("refresh", "old") is None
    assert not (await fs._db().collection("oauth_refresh").document("old").get()).exists


@firestore_only
@pytest.mark.asyncio
async def test_firestore_rejects_keys_that_are_not_document_ids(fs):
    for bad in ["", "a/b", ".", "..", "__x__", "k" * 1501]:
        with pytest.raises(ValueError):
            await fs.get("clients", bad)
    await fs.put("accounts", "github:4242", {"ok": True})  # what real keys look like
    assert await fs.get("accounts", "github:4242") == {"ok": True}


@firestore_only
def test_firestore_store_across_event_loops(fs):
    """One store used from successive event loops (the provider tests'
    asyncio.run per step) remakes its client instead of reusing a channel
    bound to a closed loop."""
    asyncio.run(fs.put("pending", "r1", {"state": "s"}))
    assert asyncio.run(fs.get("pending", "r1")) == {"state": "s"}
