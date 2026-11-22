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
from tables import LIST_LIMIT, Party, TableSessions

SCRIPT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "deploy", "cloudrun", "delete_account.py")
ME, OTHER = "github:1", "github:2"
FAR = 4_000_000_000


def _run(project, *args):
    env = {**os.environ, "GOOGLE_CLOUD_PROJECT": project}
    return subprocess.run([sys.executable, SCRIPT, "--project", project, *args], capture_output=True, text=True, env=env, timeout=60)


async def _populate(store, account, sessions):
    """What a signed-in account leaves behind: the account, its table
    sessions, an access and a refresh token."""
    await store.put("accounts", account, {"provider": "github", "provider_user_id": account[7:], "display_name": "n"})
    tables = TableSessions(store)
    for n in range(sessions):
        await tables.create(f"{account}-s{n}", account, Party.browser())
    for kind in ("access", "refresh"):
        await store.put(kind, hashlib.sha256(f"{kind}-{account}".encode()).hexdigest(), {"subject": account, "expires_at": FAR})


@pytest.mark.asyncio
async def test_dry_run_lists_then_delete_removes_only_that_account():
    if not os.environ.get("FIRESTORE_EMULATOR_HOST"):
        pytest.skip("FIRESTORE_EMULATOR_HOST not set (no Firestore emulator)")
    project = f"demo-obgl-{uuid.uuid4().hex[:12]}"
    store = FirestoreStore(project)
    extra = 2  # past the 20 sessions an account's list remembers: the script must still find them
    await _populate(store, ME, LIST_LIMIT + extra)
    await _populate(store, OTHER, 2)

    dry = _run(project, ME)
    assert dry.returncode == 0, dry.stderr
    listed = dry.stdout.splitlines()
    assert len(listed) == 2 + 2 * (LIST_LIMIT + extra) + 2  # account + its list, each session + its watch token, access + refresh
    assert all(line.startswith("would delete oauth_") for line in listed) and not any("github:2" in line for line in listed)
    assert await store.get("accounts", ME) is not None  # a dry run deletes nothing

    done = _run(project, ME, "--delete")
    assert done.returncode == 0, done.stderr
    assert await store.get("accounts", ME) is None
    assert await store.get("account-tables", ME) is None
    assert await store.get("table-sessions", f"{ME}-s0") is None
    assert await store.get("table-sessions", f"{ME}-s{LIST_LIMIT + extra - 1}") is None
    for kind in ("access", "refresh"):
        assert await store.get(kind, hashlib.sha256(f"{kind}-{ME}".encode()).hexdigest()) is None
    # The other account is whole.
    assert await store.get("accounts", OTHER) is not None
    assert len(await TableSessions(store).list_for(OTHER)) == 2
    assert await store.get("access", hashlib.sha256(f"access-{OTHER}".encode()).hexdigest()) is not None

    again = _run(project, ME)
    assert again.returncode == 1 and "no account record" in again.stderr
