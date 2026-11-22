"""deploy/local/smoke.py against a server whose OAuth issuer is not the URL
smoke is given (a Cloud Run revision tagged --no-traffic: the image's issuer
is the public domain): with --stay-on-base every request stays on that URL.
Here the issuer is a local port where nothing listens.

Needs a table (OBGL_TABLE_URL, else localhost:9777/poker/); skips without one.
"""

import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from conftest import _free_port
from table_client import TableClient

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SMOKE = os.path.join(REPO, "deploy", "local", "smoke.py")


def _table_up() -> bool:
    try:
        TableClient().info()
        return True
    except Exception:
        return False


@pytest.mark.skipif(not _table_up(), reason="no table reachable")
def test_stay_on_base_keeps_the_table_check_off_the_issuer(run_server):
    issuer = f"http://127.0.0.1:{_free_port()}"  # nothing listens here
    with run_server(OBGL_PUBLIC_URL=issuer, OBGL_AUTH_STORE="memory") as base:
        def smoke(*extra):
            return subprocess.run(
                [sys.executable, SMOKE, "--url", base, "--repo", REPO, "--require", "oauth,table", *extra],
                capture_output=True, text=True, timeout=120,
            )

        stayed = smoke("--stay-on-base")
        assert stayed.returncode == 0, stayed.stdout + stayed.stderr
        assert "table    ok" in stayed.stdout, stayed.stdout
        assert f"resource {issuer}/mcp" in stayed.stdout, stayed.stdout
        # Without it the browser follows the sign-in to the issuer, where nothing answers.
        followed = smoke()
        assert followed.returncode != 0 and "table    ok" not in followed.stdout
