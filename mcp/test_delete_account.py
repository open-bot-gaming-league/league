"""deploy/cloudrun/delete_account.py against the Firestore emulator: it lists
what holds one account's data (dry run), deletes exactly that with --delete,
and leaves every other account's records alone. Skips without
FIRESTORE_EMULATOR_HOST (the Firestore tests' emulator; see test_oauth_store.py).
"""

import hashlib
import os
import subprocess
import sys
import uuid

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from oauth.store import FirestoreStore
from programs import ProgramTokens
from tables import LIST_LIMIT

SCRIPT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "deploy", "cloudrun", "delete_account.py")
ME, OTHER = "github:1", "github:2"
FAR = 4_000_000_000


def _run(project, *args):
    env = {k: v for k, v in os.environ.items() if k not in ("OBGL_BOTS_BUCKET", "OBGL_RECORDS_BUCKET")}
    env["GOOGLE_CLOUD_PROJECT"] = project
    return subprocess.run([sys.executable, SCRIPT, "--project", project, *args], capture_output=True, text=True, env=env, timeout=60)


async def _populate(store, account, sessions, old_sessions=0):
    """What a signed-in account leaves behind: the account, its table
    sessions (the newest is its table; `old_sessions` more from before one
    table per login, each with a watch token), an access and a refresh
    token, and its program token, used."""
    await store.put("accounts", account, {"provider": "github", "provider_user_id": account[7:], "display_name": "n"})
    ids = [f"{account}-s{n}" for n in range(sessions + old_sessions)]
    for n, session_id in enumerate(ids):
        record = {"owner": account, "created_at": 1}
        if n < old_sessions:
            record["watch"] = f"watch-{session_id}"
            await store.put("watch-tokens", hashlib.sha256(record["watch"].encode()).hexdigest(), {"session": session_id})
        await store.put("table-sessions", session_id, record)
    await store.put("account-tables", account, {"sessions": list(reversed(ids))[:LIST_LIMIT]})
    for kind in ("access", "refresh"):
        await store.put(kind, hashlib.sha256(f"{kind}-{account}".encode()).hexdigest(), {"subject": account, "expires_at": FAR})
    await store.put("account-bots", account, {"versions": [], "open": f"{account}-bot"})
    await store.put("bot-submissions", f"{account}-bot", {"account": account, "state": "open"})
    await store.put("account-ranked", account, {"tables": [f"{account}-table"]})
    await store.put("ranked-tables", f"{account}-table", {"account": account, "state": "done"})
    await store.put("account-badges", account, {"badges": [{"badge": "win", "table": f"{account}-table"}]})
    programs = ProgramTokens(store)
    used = await programs.create(account, "one")
    assert await programs.check(used["token"]) is not None  # writes its program-usage record
    return used["token"]


@pytest.mark.asyncio
async def test_dry_run_lists_then_delete_removes_only_that_account():
    if not os.environ.get("FIRESTORE_EMULATOR_HOST"):
        pytest.skip("FIRESTORE_EMULATOR_HOST not set (no Firestore emulator)")
    project = f"demo-obgl-{uuid.uuid4().hex[:12]}"
    store = FirestoreStore(project)
    new, old = LIST_LIMIT, 2  # past the 20 sessions an account's list remembers: the script must still find them
    my_token = await _populate(store, ME, new, old)
    other_token = await _populate(store, OTHER, 2)

    dry = _run(project, ME)
    assert dry.returncode == 0, dry.stderr
    listed = dry.stdout.splitlines()
    # account + its list + its program list, each session (+ an old one's watch token),
    # access + refresh, the program token + its usage record, its bot list + a submission,
    # its ranked tables' list + a table, its badges
    assert len(listed) == 3 + (new + old) + old + 2 + 1 + 1 + 2 + 2 + 1, listed
    assert all(line.startswith("would delete oauth_") for line in listed) and not any("github:2" in line for line in listed)
    assert await store.get("accounts", ME) is not None  # a dry run deletes nothing

    assert "gs://BOTS_BUCKET/bots/github:1/" in dry.stderr  # no bucket named: the command says where it goes
    done = _run(project, ME, "--delete", "--bots-bucket", "the-bots", "--records-bucket", "the-records")
    assert done.returncode == 0, done.stderr
    assert await store.get("accounts", ME) is None
    assert await store.get("account-tables", ME) is None
    assert await store.get("account-programs", ME) is None
    assert await store.get("account-bots", ME) is None and await store.get("bot-submissions", f"{ME}-bot") is None
    assert "gs://the-bots/bots/github:1/" in done.stderr and "gs://the-bots/uploads/github:1/" in done.stderr
    assert f"gs://the-records/{ME}-table.json" in done.stderr
    assert await store.get("ranked-tables", f"{ME}-table") is None and await store.get("account-ranked", ME) is None
    assert await store.get("account-badges", ME) is None
    assert await store.get("table-sessions", f"{ME}-s0") is None
    assert await store.get("table-sessions", f"{ME}-s{new + old - 1}") is None
    assert await store.get("watch-tokens", hashlib.sha256(f"watch-{ME}-s0".encode()).hexdigest()) is None
    for kind in ("access", "refresh"):
        assert await store.get(kind, hashlib.sha256(f"{kind}-{ME}".encode()).hexdigest()) is None
    assert await ProgramTokens(store).check(my_token) is None
    # The other account is whole.
    assert await store.get("accounts", OTHER) is not None
    assert len((await store.get("account-tables", OTHER))["sessions"]) == 2
    assert await store.get("access", hashlib.sha256(f"access-{OTHER}".encode()).hexdigest()) is not None
    assert await ProgramTokens(store).check(other_token) is not None
    assert (await ProgramTokens(store).get(OTHER))["label"] == "one"
    assert await store.get("bot-submissions", f"{OTHER}-bot") is not None
    assert await store.get("ranked-tables", f"{OTHER}-table") is not None
    assert await store.get("account-badges", OTHER) is not None

    again = _run(project, ME)
    assert again.returncode == 1 and "no account record" in again.stderr
