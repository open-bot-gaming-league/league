"""Checking submitted bots: the work a finalize asks for (bots.py:
Bots.finalize queues the submission; Bots.check does the work), done by a
runner apart from the server's requests. Every file of the submission is
read back and its size and sha256 compared with what was declared, the
module goes through the wasm check, and then the version is filed and its
ranked table queued (ranked.py), or the submission is rejected. The coach's
program asks after it (GET .../submissions/<id>) until it is one or the
other.

One runner checks at a time: the server's own (when no OBGL_FINALIZE_JOB is
set: Finalizer.serve in the server) or the Cloud Run job's (`python
finalize.py run`, deploy/cloudrun/finalize-job.yaml), which the server
starts (jobs.py). The runner takes the queue's head under a lease on the
submission's record, renewed while it works, and takes it off the queue
when it is done. A runner stopped part way loses nothing: the next one
finds the lease run out and checks it again, up to MAX_STARTS starts, after
which the submission is rejected as interrupted.
"""

from __future__ import annotations

import argparse
import logging
import os
import time
from dataclasses import dataclass

import anyio

from bots import Bots
from jobs import held

log = logging.getLogger("obgl.finalize")

MAX_STARTS = 3


@dataclass
class Finalizer:
    bots: Bots
    lease: float = 120  # how long the head is this runner's without a renewal

    @property
    def store(self):
        return self.bots.store

    async def step(self) -> bool:
        """Check the queue's head, or settle it: False when there is nothing
        this runner can do (the queue is empty, or another runner holds its
        head)."""
        queue = await self.bots.queue.submissions()
        if not queue:
            return False
        sid = queue[0]
        sub = await self.store.get("bot-submissions", sid)
        now = self.bots.clock()
        if not self.bots.pending(sub):
            await self.bots.queue.remove(sid)  # done, or its account deleted
            return True
        if held(sub, now):
            return False
        if sub.get("attempts", 0) >= MAX_STARTS:
            if sub["state"] == "checking":
                await self.bots.reject(sid, sub, [f"the check was interrupted {sub['attempts']} times without "
                                                  f"finishing: submit the bot again"])
            else:
                log.error("finalize: %s: its ranked table was not queued after %d starts", sid, sub["attempts"])
            await self.bots.queue.remove(sid)
            return True
        seen = (sub.get("lease_until"), sub.get("attempts", 0))

        def claim(s):
            # Only the record as read: another runner that claimed it first wins.
            if s is None or (s.get("lease_until"), s.get("attempts", 0)) != seen:
                return None
            return {**s, "lease_until": now + self.lease, "attempts": seen[1] + 1}

        if await self.store.update("bot-submissions", sid, claim) is None:
            return True  # it changed under us: look again
        await self.bots.queue.taken()
        log.info("finalize: submission %s (%s of %s): start %d", sid, sub["name"], sub["account"], seen[1] + 1)
        try:
            async with anyio.create_task_group() as tg:
                async def renew():
                    while True:
                        await anyio.sleep(self.lease / 3)
                        await self._renew(sid, self.bots.clock() + self.lease)
                tg.start_soon(renew)
                await self.bots.check(sid)
                tg.cancel_scope.cancel()
        except Exception:
            # The runner's own failure (the store or the bucket down), not the
            # submission's: it waits for the next start, and this one does not count.
            await self._release(sid, count=False)
            raise
        except BaseException:
            # Stopped: the next start checks it at once; this start counts.
            with anyio.CancelScope(shield=True):
                await self._release(sid, count=True)
            raise
        await self.bots.queue.remove(sid)
        after = await self.store.get("bot-submissions", sid)
        log.info("finalize: submission %s %s%s", sid, after["state"] if after else "deleted",
                 f": {'; '.join(after['problems'])}" if after and after.get("problems") else "")
        return True

    async def drain(self, stop_after: float | None = None) -> None:
        """Check until the queue is empty (or its head is another runner's);
        with stop_after, take no new submission after that many seconds."""
        began = time.monotonic()
        while (stop_after is None or time.monotonic() - began < stop_after) and await self.step():
            pass

    async def serve(self, poll: float = 30) -> None:
        """Check submissions as they are queued, for as long as it runs; looks
        at the queue when one is queued in this process, and every poll seconds."""
        while True:
            event = self.bots.queue.wake_event()
            try:
                await self.drain()
                with anyio.move_on_after(poll):
                    await event.wait()
            except anyio.get_cancelled_exc_class():
                raise
            except Exception:
                log.exception("finalize: the runner failed; trying again in %s s", poll)
                await anyio.sleep(poll)
            finally:
                self.bots.queue.forget_event(event)

    async def _renew(self, sid: str, until: float) -> None:
        await self.store.update("bot-submissions", sid,
                                lambda s: None if s is None or "lease_until" not in s else {**s, "lease_until": until})

    async def _release(self, sid: str, count: bool) -> None:
        def change(s):
            if s is None or "lease_until" not in s:
                return None
            return {**s, "lease_until": 0, "attempts": s.get("attempts", 1) - (0 if count else 1)}
        await self.store.update("bot-submissions", sid, change)


def main() -> None:
    """The Cloud Run job's command: `python finalize.py run` checks every
    queued submission, one at a time, and exits when the queue is empty.

        OBGL_BOT_STORE, OBGL_BOTCHECK, OBGL_BOT_SUBMITS_PER_HOUR   as the server's (bot_web.py)
        OBGL_RANKED_JOB             the ranked-table job, started when a table is queued (jobs.py)
        OBGL_FINALIZE_STOP_AFTER    seconds after which it takes no new submission (the
                                    task's timeout less the longest check; default: none)
    """
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("run", help="check the queued submissions, then exit")
    ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    from badges import settings_from_env as badge_settings_from_env
    from bot_web import bots_from_env
    from jobs import starter_from_env
    from oauth.store import store_from_env
    from ranked import RankedTables

    env = os.environ
    store = store_from_env(env)
    ranked = RankedTables(store, badges=badge_settings_from_env(env), starter=starter_from_env(env, "OBGL_RANKED_JOB"))
    bots = bots_from_env(env, store, env.get("OBGL_PUBLIC_URL", ""), ranked)
    if bots is None:
        raise SystemExit("the finalize job needs OBGL_BOT_STORE")
    stop = float(env["OBGL_FINALIZE_STOP_AFTER"]) if env.get("OBGL_FINALIZE_STOP_AFTER") else None
    anyio.run(Finalizer(bots).drain, stop)


if __name__ == "__main__":
    main()
