"""The bot submission routes, on the table's API beside the table's own
(table_web.py): the same auth (a program token, or the signed-in page),
the same rate limit and error shape. bots.py has the rules; bot_files.py
the storage and, for a local store, the upload routes.

    POST /table/api/bots/submissions               {name, wasm: {size, sha256}, data: [{path, size, sha256}]}
                                                   -> a submission: {id, state, uploads: [{sha256, size, done, session?}], ...}
    GET  /table/api/bots/submissions/<id>          the same, now
    POST /table/api/bots/submissions/<id>/finalize -> the submission, state checking: queued to be checked
                                                   (finalize.py); 429 over the hourly limit
    GET  /table/api/bots/submissions/<id>          while checking: state checking; then finalized, with
                                                   bot: {name, version, ...} and table: its ranked table,
                                                   or rejected, with problems
    GET  /table/api/bots                           -> {bots: [the account's versions], open}
    GET  /table/api/tables                         -> {tables: [the account's ranked tables, newest first:
                                                   {id, bot, version, state, queued_at, position?, started_at?,
                                                   finished_at?, reason?, place?, players?, result?, url?,
                                                   badges?: [the names of the badges a done one earned]}]}
    GET  /table/api/badges                         -> the account's badges (badges.py): {season, points (this
                                                   season's), seasons: {season: points}, counts: {badge: n},
                                                   badges: [newest first], summary (the page's line)}

A finalized submission's version has a ranked table (ranked.py): the
submission's `table` is the table as the list shows it. A done table's `result`
is its results' id, `url` the page (/table/results/<id>), and table_results
(server.py) reads it aloud.

Configuration (environment):

    OBGL_BOT_STORE            local:<dir> | gcs:<bucket>; unset: no bot submissions (the routes answer 503)
    OBGL_BOTCHECK             the botcheck binary (poker/arena/cmd/botcheck); required with a store
    OBGL_BOT_SUBMITS_PER_HOUR finalized submissions an account may make in an hour (default 1; 0: no limit)
    OBGL_FINALIZE_JOB         the Cloud Run job that checks submissions (jobs.py); unset: this process checks
                              them (local runs only: in production the job does)
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import replace

from mcp.server.mcpserver import MCPServer
from starlette.requests import Request

from bot_files import LocalFiles, register_upload_routes
from bots import Bots, FinalizeQueue, GoBotCheck, Limits, Refused
from jobs import starter_from_env
from oauth.store import AuthStore
from ranked import RankedTables
from table_web import COOKIE_PATH, ApiError, TableWeb


def bots_from_env(env: Mapping[str, str], store: AuthStore, public_url: str,
                  ranked: RankedTables | None = None) -> Bots | None:
    """The bot submissions the environment sets up, or None; each version
    finalized queues a ranked table in `ranked`, if given."""
    spec = env.get("OBGL_BOT_STORE")
    if not spec:
        return None
    kind, _, where = spec.partition(":")
    if kind == "local" and where:
        files = LocalFiles(where, public_url)
    elif kind == "gcs" and where:
        from bot_files_gcs import GcsFiles

        files = GcsFiles(where)
    else:
        raise ValueError(f"OBGL_BOT_STORE must be local:<dir> or gcs:<bucket>, got {spec!r}")
    command = env.get("OBGL_BOTCHECK")
    if not command:
        raise ValueError("OBGL_BOT_STORE is set but OBGL_BOTCHECK is not: the server cannot check a submitted module")
    limits = Limits()
    if env.get("OBGL_BOT_SUBMITS_PER_HOUR"):
        limits = replace(limits, per_hour=int(env["OBGL_BOT_SUBMITS_PER_HOUR"]))
    queue = FinalizeQueue(store, starter=starter_from_env(env, "OBGL_FINALIZE_JOB"))
    return Bots(store, files, GoBotCheck(command), limits, on_finalized=ranked.enqueue if ranked else None, queue=queue)


def with_url(table: dict) -> dict:
    """A ranked table as the API shows it: a done one with its page."""
    if "result" in table:
        return {**table, "url": f"{COOKIE_PATH}/results/{table['result']}"}
    return table


def register_bot_routes(server: MCPServer, web: TableWeb, bots: Bots | None, ranked: RankedTables | None = None) -> None:
    def run(step):
        async def handler(request: Request, account: str, party) -> dict:  # noqa: ARG001
            if bots is None:
                raise ApiError(503, "bot submissions are not set up on this server")
            try:
                return await step(request, account)
            except Refused as e:
                raise ApiError(e.status, "; ".join(e.problems), problems=e.problems, **e.extra) from None
        return web.api(handler)

    async def create(request: Request, account: str) -> dict:
        return await shown(await bots.create(account, await web.json_body(request)))

    async def shown(sub: dict) -> dict:
        """A submission as the API shows it: a finalized one's table as the list does."""
        if "table" in sub:
            table = await ranked.view(sub["table"]) if ranked else None
            sub = {**sub, "table": with_url(table) if table else {"id": sub["table"]}}
        return sub

    async def status(request: Request, account: str) -> dict:
        return await shown(await bots.status(account, request.path_params["id"]))

    async def finalize(request: Request, account: str) -> dict:
        web.check_origin(request)
        return await shown(await bots.finalize(account, request.path_params["id"]))

    async def listing(request: Request, account: str) -> dict:  # noqa: ARG001
        return await bots.list(account)

    async def tables(request: Request, account: str, party) -> dict:  # noqa: ARG001
        if ranked is None:
            raise ApiError(503, "ranked tables are not set up on this server")
        return {"tables": [with_url(t) for t in (await ranked.list(account))["tables"]]}

    async def badges(request: Request, account: str, party) -> dict:  # noqa: ARG001
        if ranked is None:
            raise ApiError(503, "ranked tables are not set up on this server")
        view = await ranked.badges.view(account)
        view["badges"] = [{**b, "url": f"{COOKIE_PATH}/results/{b['table']}"} for b in view["badges"]]
        return view

    api = f"{COOKIE_PATH}/api/bots"
    server.custom_route(f"{api}/submissions", methods=["POST"])(run(create))
    server.custom_route(api + "/submissions/{id}", methods=["GET"])(run(status))
    server.custom_route(api + "/submissions/{id}/finalize", methods=["POST"])(run(finalize))
    server.custom_route(api, methods=["GET"])(run(listing))
    server.custom_route(f"{COOKIE_PATH}/api/tables", methods=["GET"])(web.api(tables))
    server.custom_route(f"{COOKIE_PATH}/api/badges", methods=["GET"])(web.api(badges))
    if bots is not None and isinstance(bots.files, LocalFiles):
        register_upload_routes(server, bots.files)
