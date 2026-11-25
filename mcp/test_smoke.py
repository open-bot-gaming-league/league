"""deploy/local/smoke.py against a server whose OAuth issuer is not the URL
smoke is given (a Cloud Run revision tagged --no-traffic: the image's issuer
is the public domain): with --stay-on-base every request stays on that URL.
Here the issuer is a local port where nothing listens. The server signs in
through a fake GitHub (smoke's --sign-in github); without --sign-in smoke only
checks the sign-in page and skips the signed-in steps (the second test).

Needs a table (OBGL_TABLE_URL, else localhost:9777/poker/); skips without one.
"""

import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from conftest import USER_A, _free_port, league_app
import oauth_testing as ot
from oauth.store import MemoryStore
from table_client import TableClient

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SMOKE = os.path.join(REPO, "deploy", "local", "smoke.py")


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


@pytest.fixture
def league(serve_asgi, free_port):
    """(base, issuer): our server on `base` signing in through a fake GitHub,
    its OAuth issuer a port where nothing listens."""
    issuer = f"http://127.0.0.1:{_free_port()}"
    with serve_asgi(ot.FakeIdP(USER_A).app) as idp_base:
        with serve_asgi(league_app(issuer, idp_base, MemoryStore()), free_port) as base:
            yield base, issuer


def _smoke(base, *extra):
    return subprocess.run(
        [sys.executable, SMOKE, "--url", base, "--repo", REPO, "--require", "oauth,table", *extra],
        capture_output=True, text=True, timeout=120,
    )


@pytest.mark.skipif(not _table_up(), reason="no table reachable")
def test_stay_on_base_keeps_the_table_check_off_the_issuer(league):
    base, issuer = league
    stayed = _smoke(base, "--sign-in", "github", "--stay-on-base")
    assert stayed.returncode == 0, stayed.stdout + stayed.stderr
    assert "table    ok: GET /table/api/info -> 200" in stayed.stdout and "watch link" in stayed.stdout, stayed.stdout
    assert f"resource {issuer}/mcp" in stayed.stdout, stayed.stdout
    # Without it the browser follows the sign-in to the issuer, where nothing answers.
    followed = _smoke(base, "--sign-in", "github")
    assert followed.returncode != 0 and "watch link" not in followed.stdout


@pytest.mark.skipif(not _table_up(), reason="no table reachable")
def test_without_sign_in_smoke_checks_the_sign_in_page_and_skips_the_rest(league):
    """What the local stack and a real deployment get: no way to sign in from a script."""
    base, _ = league
    r = _smoke(base)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "sign-in page: buttons GitHub, no guest" in r.stdout and "POST /login/guest 404" in r.stdout, r.stdout
    # The public table info is checked without a sign-in; the signed-in steps are skipped.
    assert re.search(r"table    ok: GET /table/api/info -> 200, bots \w+", r.stdout), r.stdout
    assert "mcp      skipped" in r.stdout and "table    skipped" in r.stdout, r.stdout
