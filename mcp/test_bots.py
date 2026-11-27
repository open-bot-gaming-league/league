"""Bot submission (bots.py) on a local file store (bot_files.LocalFiles):
the rules, run directly, with uploads made through LocalFiles' side of the
upload protocol. The HTTP routes and the SDK's client are test_bot_submit.py's."""

import hashlib
import os
import sys
import threading

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import anyio
import anyio.to_thread
import pytest

from bot_files import UPLOAD_PATH, LocalFiles, bot_key
from bot_web import bots_from_env
from bots import BUSY_CHECKING, DAY, HOUR, Bots, Limits, Refused
from finalize import Finalizer
from oauth.store import MemoryStore

ALICE, BOB = "github:1001", "github:2002"
WASM = b"\0asm\x01\0\0\0"  # the empty module: valid WebAssembly


class Clock:
    def __init__(self):
        self.t = 1_800_000_000.0

    def __call__(self):
        return self.t


def accept(wasm: bytes) -> str | None:
    return None if b"refuse" not in wasm else "it exports no bot (a test module)"


@pytest.fixture
def env(tmp_path):
    clock = Clock()
    files = LocalFiles(str(tmp_path / "bots"), "http://league.test")
    return Bots(MemoryStore(), files, accept, Limits(wasm_bytes=1 << 20, data_bytes=1 << 20, data_files=4), clock), clock


def entry(data: bytes) -> dict:
    return {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def manifest(name="mybot", wasm=WASM, data=None) -> dict:
    return {"name": name, "wasm": entry(wasm), "data": [{"path": p, **entry(b)} for p, b in (data or {}).items()]}


def session_id(session: str) -> str:
    assert session.startswith("http://league.test" + UPLOAD_PATH), session
    return session.rsplit("/", 1)[1]


def upload(files: LocalFiles, session: str, data: bytes, chunk: int = 5) -> None:
    """The client's side of the protocol: PUT the bytes to the session in chunks."""
    for a in range(0, len(data), chunk):
        piece = data[a:a + chunk]
        status, _, error = files.put(session_id(session), f"bytes {a}-{a + len(piece) - 1}/{len(data)}", iter([piece]))
        assert status in (200, 308), error
    assert status == 200


def upload_all(bots: Bots, sub: dict, blobs: dict[str, bytes]) -> None:
    for u in sub["uploads"]:
        if not u["done"]:
            upload(bots.files, u["session"], blobs[u["sha256"]])


def by_sha(*blobs: bytes) -> dict[str, bytes]:
    return {hashlib.sha256(b).hexdigest(): b for b in blobs}


async def checked(bots: Bots, account: str, sid: str) -> dict:
    """Finalize, and the check it queues done (finalize.py's runner, here):
    the submission as it ends, or Refused(422) with why it was rejected."""
    asked = await bots.finalize(account, sid)
    assert asked["state"] == "checking" and await bots.queue.submissions() == [sid]
    await Finalizer(bots).drain()
    assert await bots.queue.submissions() == []
    done = await bots.status(account, sid)
    if done["state"] == "rejected":
        raise Refused(422, done["problems"])
    assert done["state"] == "finalized", done
    return done


async def submit(bots: Bots, account: str, wasm=WASM, data=None, name="mybot") -> dict:
    sub = await bots.create(account, manifest(name, wasm, data))
    upload_all(bots, sub, by_sha(wasm, *(data or {}).values()))
    return await checked(bots, account, sub["id"])


@pytest.mark.asyncio
async def test_a_submission_becomes_a_version_and_its_files_are_kept_by_sha256(env):
    bots, _ = env
    data = {"tables/a.bin": b"alpha" * 7, "b.bin": b"beta"}
    sub = await bots.create(ALICE, manifest(data=data))
    assert sub["state"] == "open" and [f["path"] for f in sub["data"]] == ["b.bin", "tables/a.bin"]
    assert {u["sha256"] for u in sub["uploads"]} == set(by_sha(WASM, *data.values())) and not any(u["done"] for u in sub["uploads"])
    upload_all(bots, sub, by_sha(WASM, *data.values()))
    assert all(u["done"] and "session" not in u for u in (await bots.status(ALICE, sub["id"]))["uploads"])
    bot = (await checked(bots, ALICE, sub["id"]))["bot"]
    assert (bot["name"], bot["version"], bot["data_files"], bot["data_bytes"]) == ("mybot", 1, 2, 39)
    assert bot["wasm"] == entry(WASM)
    for blob in by_sha(WASM, *data.values()):
        assert bots.files.size(bot_key(ALICE, blob)) is not None
    assert not os.path.exists(os.path.join(bots.files.root, "uploads"))  # nothing unchecked left behind
    listed = await bots.list(ALICE)
    assert listed == {"bots": [bot], "open": None}
    assert await bots.list(BOB) == {"bots": [], "open": None}
    assert (await bots.status(ALICE, sub["id"]))["state"] == "finalized"
    with pytest.raises(Refused) as e:
        await bots.status(BOB, sub["id"])  # someone else's submission is no submission
    assert e.value.status == 404


@pytest.mark.asyncio
async def test_the_next_version_uploads_only_what_the_account_does_not_have(env):
    bots, clock = env
    data = {"big.bin": b"x" * 1000}
    await submit(bots, ALICE, data=data)
    clock.t += HOUR
    wasm2 = WASM + b"\0\x04name"  # another valid module (a custom section)
    sub = await bots.create(ALICE, manifest(wasm=wasm2, data=data))
    assert [u["sha256"] for u in sub["uploads"]] == [entry(wasm2)["sha256"]]
    upload_all(bots, sub, by_sha(wasm2))
    assert (await checked(bots, ALICE, sub["id"]))["bot"]["version"] == 2
    # Bob's identical data is his to upload: no account sees another's files.
    sub = await bots.create(BOB, manifest(data=data))
    assert len(sub["uploads"]) == 2


@pytest.mark.asyncio
async def test_finalize_refuses_a_file_whose_bytes_are_not_the_ones_declared(env):
    bots, _ = env
    good, bad = b"the declared bytes", b"the uploaded bytes"  # same length, other sha256
    sub = await bots.create(ALICE, manifest(data={"d.bin": good}))
    for u in sub["uploads"]:
        upload(bots.files, u["session"], bad if u["sha256"] == entry(good)["sha256"] else WASM)
    with pytest.raises(Refused) as e:
        await checked(bots, ALICE, sub["id"])
    assert e.value.status == 422 and e.value.problems == [
        "data file 'd.bin': its sha256 is not the one declared (the file changed, or the upload was damaged)"]
    assert (await bots.status(ALICE, sub["id"]))["state"] == "rejected"
    assert bots.files.size(bot_key(ALICE, entry(WASM)["sha256"])) is None  # nothing of it kept
    assert (await bots.list(ALICE)) == {"bots": [], "open": None}
    with pytest.raises(Refused) as e:
        await bots.finalize(ALICE, sub["id"])
    assert e.value.status == 409 and "rejected" in e.value.problems[0]


@pytest.mark.asyncio
async def test_finalize_names_what_is_missing_or_short(env):
    bots, _ = env
    data = {"a.bin": b"a" * 20, "b.bin": b"b" * 20}
    sub = await bots.create(ALICE, manifest(data=data))
    sessions = {u["sha256"]: u["session"] for u in sub["uploads"]}
    upload(bots.files, sessions[entry(WASM)["sha256"]], WASM)
    half = session_id(sessions[entry(data["a.bin"])["sha256"]])
    assert bots.files.put(half, "bytes 0-9/20", iter([b"a" * 10]))[:2] == (308, {"Range": "bytes=0-9"})
    with pytest.raises(Refused) as e:
        await checked(bots, ALICE, sub["id"])
    assert e.value.problems == ["data file 'a.bin': not uploaded", "data file 'b.bin': not uploaded"]


@pytest.mark.asyncio
async def test_finalize_refuses_what_is_not_a_bot_module(env):
    bots, clock = env
    for wasm, problem in ((b"MZ\x90\0 a program for another machine", "wasm: not a WebAssembly module (it does not start with \\0asm)"),
                          (WASM + b"\0\x07refuse", "wasm: it exports no bot (a test module)")):
        clock.t += HOUR  # each a new hour: a refused one counts
        with pytest.raises(Refused) as e:
            await submit(bots, ALICE, wasm=wasm)
        assert e.value.status == 422 and e.value.problems == [problem]
    assert (await bots.list(ALICE))["bots"] == []


@pytest.mark.asyncio
async def test_the_caps_and_the_manifest_are_checked_before_any_upload(env):
    bots, _ = env
    body = manifest(data={"a": b"1"})
    body["wasm"]["size"] = (1 << 20) + 1
    body["data"] += [{"path": p, "size": 1 << 19, "sha256": "0" * 64} for p in ("b", "c", "d", "e")]
    body["data"].append({"path": "../escape", "size": 1, "sha256": "f" * 64})
    with pytest.raises(Refused) as e:
        await bots.create(ALICE, body)
    assert e.value.status == 400
    assert e.value.problems == [
        "wasm: 1,048,577 bytes is over the 1,048,576-byte cap",
        "data: 6 files is over the 4-file cap",
        "data: 1,572,865 bytes in all is over the 1,048,576-byte cap",
    ]
    bad = {"name": "My Bot", "wasm": {"size": 8, "sha256": "ABC"},
           "data": [{"path": ".hidden", "size": 1, "sha256": "0" * 64}, {"path": "x", "size": -1, "sha256": "0" * 64},
                    {"path": "x", "size": 1, "sha256": "0" * 64}]}
    with pytest.raises(Refused) as e:
        await bots.create(ALICE, bad)
    assert [p.split(":")[0] for p in e.value.problems] == ["name", "wasm", "data file '.hidden'", "data file 'x'", "data file 'x'"]
    assert e.value.problems[-1] == "data file 'x': listed twice"
    with pytest.raises(Refused) as e:
        await bots.create(ALICE, manifest(data={"t": b"1", "t/u/v.bin": b"2", "w/x": b"3"}))
    assert e.value.problems == ["data file 't/u/v.bin': its folder is also a file of the bot's"]
    assert (await bots.list(ALICE))["open"] is None


@pytest.mark.asyncio
async def test_a_filed_version_is_handed_on(env):
    bots, _ = env
    handed = []

    async def queue(account, version):
        handed.append((account, version["name"], version["version"]))
        return {"id": "t1", "state": "queued"}

    bots.on_finalized = queue
    done = await submit(bots, ALICE)
    assert done["table"] == "t1" and handed == [(ALICE, "mybot", 1)]


@pytest.mark.asyncio
async def test_one_version_an_hour_per_account(env):
    bots, clock = env
    await submit(bots, ALICE)
    clock.t += HOUR - 60
    with pytest.raises(Refused) as e:
        await bots.create(ALICE, manifest(name="other"))
    assert e.value.status == 429 and e.value.problems == ["You can submit one bot an hour, refused ones included: try again at 09:00 UTC"]
    assert e.value.extra == {"retry_at": int(clock.t + 60)}
    await submit(bots, BOB)  # the limit is per account
    clock.t += 60
    sub = await bots.create(ALICE, manifest(name="other"))
    upload_all(bots, sub, by_sha(WASM))
    assert (await checked(bots, ALICE, sub["id"]))["bot"]["name"] == "other"


@pytest.mark.asyncio
async def test_a_submission_the_check_refuses_still_counts_for_the_hour(env):
    bots, clock = env
    with pytest.raises(Refused):
        await submit(bots, ALICE, wasm=WASM + b"\0\x07refuse")
    await bots.create(BOB, manifest())  # Bob's hour is his own
    with pytest.raises(Refused) as e:
        await bots.create(ALICE, manifest())
    assert e.value.status == 429 and "refused ones included" in e.value.problems[0]
    clock.t += HOUR
    assert (await submit(bots, ALICE))["bot"]["version"] == 1


@pytest.mark.asyncio
async def test_a_refused_finalize_over_the_limit_leaves_the_submission_open(env, tmp_path):
    clock = Clock()
    bots = Bots(MemoryStore(), LocalFiles(str(tmp_path / "b2"), "http://league.test"), accept, Limits(per_hour=1), clock)
    sub = await bots.create(ALICE, manifest())
    upload_all(bots, sub, by_sha(WASM))
    await bots.store.put("account-bots", ALICE, {**await bots.store.get("account-bots", ALICE),
                                                "asked": [int(clock.t)]})
    with pytest.raises(Refused) as e:
        await bots.finalize(ALICE, sub["id"])
    assert e.value.status == 429
    assert (await bots.status(ALICE, sub["id"]))["state"] == "open"
    assert await bots.queue.submissions() == []  # refused before it was queued
    unlimited = Bots(bots.store, bots.files, accept, Limits(per_hour=0), clock)
    assert (await checked(unlimited, ALICE, sub["id"]))["bot"]["version"] == 1


@pytest.mark.asyncio
async def test_the_same_files_again_pick_up_the_open_submission_and_others_replace_it(env):
    bots, _ = env
    data = {"a.bin": b"a" * 30}
    first = await bots.create(ALICE, manifest(data=data))
    sessions = {u["sha256"]: u["session"] for u in first["uploads"]}
    upload(bots.files, sessions[entry(WASM)["sha256"]], WASM)
    a_bin = entry(data["a.bin"])["sha256"]
    bots.files.put(session_id(sessions[a_bin]), "bytes 0-9/30", iter([b"a" * 10]))
    again = await bots.create(ALICE, manifest(data=data))
    assert again["id"] == first["id"]
    # The same session for the file part sent: the client resumes it.
    assert again["uploads"] == [{"sha256": entry(WASM)["sha256"], "size": len(WASM), "done": True},
                                {"sha256": a_bin, "size": 30, "done": False, "session": sessions[a_bin]}]
    assert bots.files.put(session_id(sessions[a_bin]), "bytes */30", None)[:2] == (308, {"Range": "bytes=0-9"})
    other = await bots.create(ALICE, manifest(data={"a.bin": b"b" * 30}))
    assert other["id"] != first["id"]
    assert (await bots.status(ALICE, first["id"]))["state"] == "superseded"
    assert not os.path.exists(os.path.join(bots.files.root, "uploads", ALICE, first["id"]))
    assert (await bots.list(ALICE))["open"] == other["id"]


@pytest.mark.asyncio
async def test_an_open_submission_expires(env):
    bots, clock = env
    sub = await bots.create(ALICE, manifest())
    clock.t += 7 * DAY
    with pytest.raises(Refused) as e:
        await bots.finalize(ALICE, sub["id"])
    assert e.value.status == 409 and "expired" in e.value.problems[0]


def test_local_upload_sessions_resume_and_keep_to_their_size(tmp_path):
    files = LocalFiles(str(tmp_path), "http://league.test")
    key = "uploads/github:1001/" + "cd" * 16 + "/" + "ab" * 32
    upload_id = session_id(files.start_upload(key, 10))
    assert files.put("0" * 32, "bytes */10", None)[0] == 404  # no such session
    put = lambda rng, body=None: files.put(upload_id, rng, None if body is None else iter([body]))[:2]
    assert put("bytes */10") == (308, {})
    assert put("bytes 0-5/10", b"012345") == (308, {"Range": "bytes=0-5"})
    assert put("bytes 0-5/10", b"012345") == (308, {"Range": "bytes=0-5"})  # a resent chunk: the bytes it has are skipped
    assert put("bytes 8-9/10", b"89")[0] == 400  # past what it has: refused
    assert put("bytes 4-11/12", b"45678901")[0] == 400  # not the size it was started for
    assert put("bytes 4-9/10", b"4567890")[0] == 400  # more bytes than the range
    assert put("bytes 4-9/10", b"456789") == (200, {})
    assert files.size(key) == 10 and b"".join(files.read(key)) == b"0123456789"
    assert put("bytes */10")[0] == 404  # the session is over
    other = session_id(files.start_upload(key.replace("ab", "ef"), 4))
    files.put(other, "bytes 0-1/4", iter([b"01"]))
    files.delete(key.replace("ab", "ef"))  # ends the session too
    assert files.put(other, "bytes */4", None)[0] == 404
    assert os.listdir(os.path.join(files.root, ".sessions")) == []


class Racing(MemoryStore):
    """A store that runs `before` (once) just before its next account-bots update."""

    before = None

    async def update(self, kind, key, change):
        if kind == "account-bots" and self.before is not None:
            before, self.before = self.before, None
            await before()
        return await super().update(kind, key, change)


@pytest.mark.asyncio
async def test_the_hourly_limit_is_counted_when_finalize_is_asked_on_any_server(tmp_path):
    clock, store = Clock(), Racing()
    files = LocalFiles(str(tmp_path / "bots"), "http://league.test")
    one, two = (Bots(store, files, accept, Limits(per_hour=3), clock) for _ in "12")  # two servers, one store
    await submit(one, ALICE)
    b = await one.create(ALICE, manifest(name="b"))
    upload_all(one, b, by_sha(WASM))
    assert (await one.finalize(ALICE, b["id"]))["state"] == "checking"
    c = await two.create(ALICE, manifest(name="c"))
    upload_all(two, c, by_sha(WASM))
    with pytest.raises(Refused) as e:
        await two.finalize(ALICE, c["id"])  # one at a time per account
    assert e.value.status == 409 and e.value.problems == [BUSY_CHECKING]
    await Finalizer(one).drain()
    assert (await checked(two, ALICE, c["id"]))["bot"]["name"] == "c"
    with pytest.raises(Refused) as e:
        await one.create(ALICE, manifest(name="e"))  # three asked this hour, on either server
    assert e.value.status == 429 and e.value.problems == ["You can submit 3 bots an hour, refused ones included: try again at 09:00 UTC"]
    assert await two.queue.submissions() == []

    # Two servers asking at once: the store's conditional write lets one in.
    clock.t += HOUR
    d = await one.create(ALICE, manifest(name="d"))
    upload_all(one, d, by_sha(WASM))
    store.before = lambda: two.finalize(ALICE, d["id"])  # server two asks between one's look and its write
    with pytest.raises(Refused) as e:
        await one.finalize(ALICE, d["id"])
    assert e.value.status == 409 and e.value.problems == [BUSY_CHECKING]
    assert await one.queue.submissions() == [d["id"]]  # queued once, by server two


def test_the_hourly_limit_comes_from_the_environment(tmp_path):
    base = {"OBGL_BOT_STORE": f"local:{tmp_path}", "OBGL_BOTCHECK": "/bin/false"}
    assert bots_from_env(base, MemoryStore(), "http://league.test").limits.per_hour == 1
    assert bots_from_env({**base, "OBGL_BOT_SUBMITS_PER_HOUR": "3"}, MemoryStore(), "http://league.test").limits.per_hour == 3
    assert bots_from_env({**base, "OBGL_BOT_SUBMITS_PER_HOUR": "0"}, MemoryStore(), "http://league.test").limits.per_hour == 0
