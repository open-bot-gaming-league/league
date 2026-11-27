"""The bot submission routes, on the table's API beside the table's own
(table_web.py): the same auth (a program token, or the signed-in page),
the same rate limit and error shape. bots.py has the rules; bot_files.py
the storage and, for a local store, the upload routes.

    POST /table/api/bots/submissions               {name, wasm: {size, sha256}, data: [{path, size, sha256}]}
                                                   -> a submission: {id, state, uploads: [{sha256, size, done, session?}], ...}
    GET  /table/api/bots/submissions/<id>          the same, now
    POST /table/api/bots/submissions/<id>/finalize -> {bot: {name, version, ...}}; 422 {error, problems}
    GET  /table/api/bots                           -> {bots: [the account's versions], open}

Configuration (environment):

    OBGL_BOT_STORE            local:<dir> | gcs:<bucket>; unset: no bot submissions (the routes answer 503)
    OBGL_BOTCHECK             the botcheck binary (poker/arena/cmd/botcheck); required with a store
    OBGL_BOT_SUBMITS_PER_DAY  finalized submissions an account may make in 24 hours (default 1; 0: no limit)
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import replace

from mcp.server.mcpserver import MCPServer
from starlette.requests import Request

from bot_files import LocalFiles, register_upload_routes
from bots import Bots, GoBotCheck, Limits, Refused
from oauth.store import AuthStore
from table_web import COOKIE_PATH, ApiError, TableWeb


def bots_from_env(env: Mapping[str, str], store: AuthStore, public_url: str) -> Bots | None:
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
    if env.get("OBGL_BOT_SUBMITS_PER_DAY"):
        limits = replace(limits, per_day=int(env["OBGL_BOT_SUBMITS_PER_DAY"]))
    return Bots(store, files, GoBotCheck(command), limits)


def register_bot_routes(server: MCPServer, web: TableWeb, bots: Bots | None) -> None:
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
        return await bots.create(account, await web.json_body(request))

    async def status(request: Request, account: str) -> dict:
        return await bots.status(account, request.path_params["id"])

    async def finalize(request: Request, account: str) -> dict:
        web.check_origin(request)
        return await bots.finalize(account, request.path_params["id"])

    async def listing(request: Request, account: str) -> dict:  # noqa: ARG001
        return await bots.list(account)

    api = f"{COOKIE_PATH}/api/bots"
    server.custom_route(f"{api}/submissions", methods=["POST"])(run(create))
    server.custom_route(api + "/submissions/{id}", methods=["GET"])(run(status))
    server.custom_route(api + "/submissions/{id}/finalize", methods=["POST"])(run(finalize))
    server.custom_route(api, methods=["GET"])(run(listing))
    if bots is not None and isinstance(bots.files, LocalFiles):
        register_upload_routes(server, bots.files)
