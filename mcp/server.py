"""MCP server for Bot Poker: seven tools (docs, rules, new_session, state,
act, next_hand, score): six wrap the live table's HTTP API, docs() returns
the documents. See
docs/AGENTS.md for what this is and how to read it; for the stdio
mode, add it to your MCP client config as a stdio server (command
`python`, args `[path/to/this/file]`).

Two transports, one server:

- **stdio** (default) -- one process serves exactly one client, so all
  tool calls share a single `PokerSession`. This is what Claude
  Desktop and a local `python server.py` get.
- **Streamable HTTP** (`OBGL_TRANSPORT=http`) -- the transport
  claude.ai custom connectors speak, and what deploy/cloudrun/ serves.
  One running server can hold many concurrent client connections (one
  per claude.ai conversation), each with its own `mcp-session-id`, so
  poker state here is kept per `mcp-session-id` -- see `_poker_for`
  below -- or two different conversations would silently share one
  poker session and stomp each other's hands.

`_poker_for(ctx)` is what makes one `build_server()` correct under
both: it keys a `PokerSession` off the transport's own per-connection
identity when the transport has one (`ctx.headers` is not `None`), and
falls back to a single shared session when it doesn't (stdio has no
headers at all -- `getattr(request, "headers", None)` on its request
object misses -- and stdio only ever has the one client anyway, so a
shared fallback is exactly as correct as the old stdio-only server's
module-level `PokerSession` was).

Tool bodies otherwise do exactly what they always did; summarize_state
and PokerSession live in session.py, table_client.py is the HTTP
client -- neither has anything transport-specific in it.

Run:
  python server.py                                   # stdio (default)
  OBGL_TRANSPORT=http PORT=8080 python server.py      # Streamable HTTP

Over HTTP the same process also serves the docs as plain web pages
(see `_docs_routes`): a landing page at /, an index at /docs/, and the
raw markdown at /docs/README.md, /docs/AGENTS.md, /docs/WAYS-TO-PLAY.md,
read from the repo files on every request. A client that cannot reach
GitHub (claude.ai's fetcher was rate-limited there) still gets them
from here, or from the docs() tool.

Over HTTP with OBGL_PUBLIC_URL set, OAuth is on (oauth/__init__.py):
this server is the connector's OAuth authorization server, /mcp needs a
bearer token, and /login is the sign-in page (GitHub, Google, guest).
Each poker session then belongs to the signed-in account as well as to
its connection (see `_poker_for`). Unset: no auth, as before.

OBGL_TABLE_URL points at the table; defaults to
http://localhost:9777/poker/ for a local non-container run. Inside the
deploy/cloudrun/ container the entrypoint sets it to the table's own
in-container address.
"""

from __future__ import annotations

import os
import sys

import anyio.to_thread

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from mcp.server.auth.middleware.auth_context import get_access_token  # noqa: E402
from mcp.server.mcpserver import Context, MCPServer  # noqa: E402
from mcp.server.mcpserver.exceptions import ToolError  # noqa: E402
from mcp.server.streamable_http import MCP_SESSION_ID_HEADER  # noqa: E402
from mcp_types import DiscoverResult, RequestParams  # noqa: E402
from starlette.requests import Request  # noqa: E402
from starlette.responses import HTMLResponse, JSONResponse, Response  # noqa: E402

from oauth import OAuthConfig, register_login_routes  # noqa: E402
from pages import render  # noqa: E402
from session import (  # noqa: E402
    InvalidActionError,
    NoSessionError,
    PokerSession,
    StaleStateError,
    summarize_state,
)
from table_client import TableClient, TableError  # noqa: E402

_REPO_DIR = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
DOCS_DIR = os.path.join(_REPO_DIR, "docs")

# The documents docs() and /docs/<file> serve: tool name -> (url filename,
# path on disk). README.md lives at the repo root, the rest in docs/; the
# container image copies both (deploy/cloudrun/Dockerfile), keeping this
# relative layout. "rules" is not a file: see RULES_TEXT, served by docs().
DOC_FILES = {
    "readme": ("README.md", os.path.join(_REPO_DIR, "README.md")),
    "agents": ("AGENTS.md", os.path.join(DOCS_DIR, "AGENTS.md")),
    "ways-to-play": ("WAYS-TO-PLAY.md", os.path.join(DOCS_DIR, "WAYS-TO-PLAY.md")),
}
DOC_NAMES = [*DOC_FILES, "rules"]

# A fixed key for transports with no per-connection identity of their
# own (stdio): there is only ever one client, so every tool call shares
# one PokerSession, same as the original stdio-only server.
_SHARED_SESSION_KEY = "shared"

RULES_TEXT = """Bot Poker: heads-up, 50 bb starting stacks, stacks reset every hand.
Blinds: small blind 50 chips (0.5 bb), big blind 100 chips (1 bb) -- "bb"
below always means one big blind, 100 chips. Seats are "small blind
(button)" (acts first before the flop, last after) and "big blind".

Cards are two characters, rank then suit: `As`, `Td`, `2c`. Ranks:
2-9, T, J, Q, K, A. Suits: s, h, d, c. The board shows 0 (before the
flop), 3 (flop), 4 (turn) or 5 (river, and at a showdown/all-in) cards.

The bet menu (from api/info, read live so it stays correct if it
changes): a round's first bet is 0.5x, 1x or 2x pot; a raise is 1x pot;
a re-raise is 1x pot; all-in is always offered. Actions are chosen by
`index` into the `actions` list the last state gave you -- never guess
an index, read it from state().

Each poker tool (state/act/next_hand/score) returns the same compact
shape: hand, step, round, seat, opponent, you, bot, board, pot, over,
actions, lines, and (once a hand is over) outcome and net. `act()` fills
in which hand/step you mean from the last state this session saw, so you
never need to track those yourself -- just call state() if a result
looks unexpected before acting again.
"""

MCP_PATH = "/mcp"
CONNECTOR_URL = "https://obgl.truemirrorgames.com" + MCP_PATH
GITHUB_URL = "https://github.com/open-bot-gaming-league/league"

_DOC_LINKS = """<ul>
<li><a href="/docs/README.md">README.md</a>: what this is and how to play</li>
<li><a href="/docs/AGENTS.md">AGENTS.md</a>: the agent's entry point: tools, rules, notation</li>
<li><a href="/docs/WAYS-TO-PLAY.md">WAYS-TO-PLAY.md</a>: the ladder of ways to play</li>
</ul>"""

_SIGN_IN_LINE = {
    False: "No signup.",
    True: "Claude will ask you to sign in: with GitHub, with Google, or choose "
    "<em>Continue as guest</em> (no signup).",
}


def landing_html(oauth: bool) -> str:
    return render(
        "Open Bot Gaming League",
        f"""<h1>Open Bot Gaming League</h1>
<p>Heads-up no-limit hold'em for bots, all play money. To play, add
<code>{CONNECTOR_URL}</code> as a custom connector in Claude (Settings &gt;
Connectors &gt; Add custom connector), then ask Claude to play a hand against
one of our house bots. {_SIGN_IN_LINE[oauth]}</p>
<ul>
<li><a href="/docs/">Docs</a> (README, AGENTS, WAYS-TO-PLAY)</li>
<li><a href="{GITHUB_URL}">Source on GitHub</a></li>
</ul>""",
    )


DOCS_INDEX_HTML = render(
    "OBGL docs",
    f"""<h1>OBGL docs</h1>
<p>Raw markdown, served from the same release as the connector.</p>
{_DOC_LINKS}
<p><a href="/">Back</a> &middot; <a href="{GITHUB_URL}">GitHub</a></p>""",
)


def read_doc(name: str) -> str:
    """The text of one document by docs() name. ValueError (listing the
    valid names) for an unknown one."""
    if name == "rules":
        return RULES_TEXT
    entry = DOC_FILES.get(name)
    if entry is None:
        raise ValueError(f"unknown doc {name!r}: choose one of {', '.join(DOC_NAMES)}")
    with open(entry[1], encoding="utf-8") as f:
        return f.read()


def _register_docs_routes(server: MCPServer, oauth: bool) -> None:
    """The docs as plain web pages next to /mcp (HTTP transport only in
    practice, harmless under stdio). Files are read as bytes on every
    request so what is served is exactly what is in the repo/image."""
    landing_page = landing_html(oauth)

    @server.custom_route("/", methods=["GET"])
    async def landing(request: Request) -> Response:  # noqa: ARG001
        return HTMLResponse(landing_page)

    # Both spellings answer directly: Starlette's slash redirect would be
    # built from the request scheme, which behind Cloud Run's TLS
    # termination is http://, not https://.
    for index_path in ("/docs", "/docs/"):

        @server.custom_route(index_path, methods=["GET"])
        async def docs_index(request: Request) -> Response:  # noqa: ARG001
            return HTMLResponse(DOCS_INDEX_HTML)

    def _file_route(filename: str, path: str) -> None:
        @server.custom_route(f"/docs/{filename}", methods=["GET"])
        async def serve(request: Request) -> Response:  # noqa: ARG001
            with open(path, "rb") as f:
                body = f.read()
            return Response(body, media_type="text/markdown; charset=utf-8")

    for filename, path in DOC_FILES.values():
        _file_route(filename, path)


def build_server(base_url: str | None = None, oauth: OAuthConfig | None = None) -> MCPServer:
    client = TableClient(base_url) if base_url else TableClient()

    # One PokerSession per connection that has one (see _SHARED_SESSION_KEY
    # and the module docstring for stdio's fallback). `ctx.session` (the
    # SDK's ServerSession) is *not* usable as the per-connection key over
    # HTTP: it is a fresh, per-request proxy object (see mcp/server/
    # session.py's own docstring: "A per-request proxy built by the
    # kernel for each inbound request"), so it differs between two
    # requests on the same connection. The stable, public, per-connection
    # handle over HTTP is the transport's own `mcp-session-id` header
    # (`ctx.headers`), which every SDK client resends on every request
    # after the one that received it from `initialize`'s response.
    #
    # With OAuth on, a poker session also belongs to the signed-in
    # account (the access token's subject: "github:<id>", "google:<sub>",
    # "guest:<random>"), and the key says so: "<account>/<mcp-session-id>".
    # The SDK already refuses a request whose token's (client, issuer,
    # subject) differs from the one that opened the mcp session
    # (streamable_http_manager.py: "Session not found"), so this is
    # ownership made explicit, not the only guard.
    poker_by_key: dict[str, PokerSession] = {}  # leaks on idle/disconnect; see deploy/cloudrun/CLOUDRUN.md

    def _account() -> str | None:
        """The signed-in account calling this tool ("github:<id>",
        "google:<sub>", "guest:<random>"), from the request's access token;
        None with OAuth off. Display name etc.: provider.account(id)."""
        if oauth is None:
            return None
        token = get_access_token()
        if token is None or not token.subject:
            # The bearer check in front of /mcp makes this unreachable;
            # refuse rather than run unowned.
            raise RuntimeError("OAuth is on but this request carries no signed-in account")
        return token.subject

    def _poker_for(ctx: Context) -> PokerSession:
        headers = ctx.headers
        if headers is None:
            # No headers at all on this transport's request object
            # (stdio): one process serves one client, so one shared
            # session is correct -- there is nothing to key per-caller.
            key = _SHARED_SESSION_KEY
        else:
            session_id = headers.get(MCP_SESSION_ID_HEADER)
            if not session_id:
                # Only expected with stateless_http=True (not used here)
                # or a non-conformant client. Fail loud rather than
                # silently fusing two callers' poker sessions.
                raise RuntimeError(
                    f"no {MCP_SESSION_ID_HEADER!r} header on this request: cannot "
                    "scope poker state to a connection"
                )
            key = session_id
            account = _account()
            if account is not None:
                key = f"{account}/{session_id}"
        poker = poker_by_key.get(key)
        if poker is None:
            poker = PokerSession(client)
            poker_by_key[key] = poker
        return poker

    provider = oauth.provider() if oauth else None
    server = MCPServer(
        name="obgl-bot-poker",
        auth=oauth.auth_settings() if oauth else None,
        auth_server_provider=provider,
        instructions=(
            "Play Bot Poker (50 bb heads-up) at the live table. Call "
            "docs('agents') first, then rules(), then new_session(). "
            "docs() also has 'readme', 'ways-to-play' and 'rules'."
        ),
    )
    _register_docs_routes(server, oauth=oauth is not None)
    if provider is not None:
        register_login_routes(server, provider)

    # mcp 2.3.0 also speaks a newer "modern" Streamable HTTP mode
    # (protocol version 2026-07-28): a self-contained POST per call, no
    # `initialize` handshake, no `Mcp-Session-Id` -- see
    # mcp/server/_streamable_http_modern.py's module docstring. An
    # auto-negotiating client probes `server/discover` for it before
    # every connection. That mode has no notion of a connection at all,
    # so _poker_for(ctx) above has nothing to key on over HTTP -- the
    # whole "act() fills in hand/step from the last state this session
    # saw" design in session.py is unimplementable without session
    # continuity. Overriding `server/discover` to advertise no modern
    # version is the SDK's documented way for a stateful server to opt
    # out: the client reads that as "legacy advertisement, not an
    # incompatibility" (mcp/client/_probe.py) and falls back to the
    # classic handshake, which does carry `Mcp-Session-Id`. Same
    # mechanism the probe module says the Go and TypeScript SDKs'
    # stateful servers use. Harmless under stdio, which never sends
    # this request.
    async def _discover_legacy_only(ctx, params):  # noqa: ANN001, ARG001
        return DiscoverResult(
            supported_versions=[],
            capabilities=server._lowlevel_server.get_capabilities(protocol_version=ctx.protocol_version),
            instructions=server.instructions,
        )

    server._lowlevel_server.add_request_handler("server/discover", RequestParams, _discover_legacy_only)

    @server.resource(
        "obgl://docs/AGENTS.md",
        name="AGENTS.md",
        description="What this is, the tools, the rules of Bot Poker, notation. Read first.",
        mime_type="text/markdown",
    )
    def agents_doc() -> str:
        return read_doc("agents")

    @server.resource(
        "obgl://docs/WAYS-TO-PLAY.md",
        name="WAYS-TO-PLAY.md",
        description="The ladder of ways to play Bot Poker, and where this MCP server sits on it.",
        mime_type="text/markdown",
    )
    def ways_to_play_doc() -> str:
        return read_doc("ways-to-play")

    @server.tool()
    def docs(name: str) -> str:
        """A document as text. name: 'agents' (the entry point: read this
        first), 'readme', 'ways-to-play', or 'rules' (the static rules
        text; rules() adds the live bet menu and opponents)."""
        try:
            return read_doc(name)
        except ValueError as e:
            raise ToolError(str(e)) from e

    @server.tool()
    def rules() -> dict:
        """The rules of Bot Poker and the current opponents on offer.
        Call docs('agents') first; this tool is the live part (bet
        menu, opponent list) that can change under that doc."""
        info = client.info()
        return {
            "text": RULES_TEXT,
            "stackBB": info["stackBB"],
            "smallBlind": info["smallBlind"],
            "bigBlind": info["bigBlind"],
            "menu": info["menu"],
            "bots": info["bots"],
            "docs": ["obgl://docs/AGENTS.md", "obgl://docs/WAYS-TO-PLAY.md"],
        }

    @server.tool()
    async def new_session(ctx: Context, opponent: str | None = None, seat: str | None = None) -> dict:
        """Start a new session and deal hand 1. opponent: a bot id from
        rules()'s `bots` (default: the first/least-exploitable). seat:
        "sb", "bb" or "alternate" (default -- alternates seat each
        hand, so neither seat's advantage dominates the score)."""
        poker = _poker_for(ctx)
        account = _account()
        try:
            # The table call blocks (urllib); keep it off the event loop,
            # as the SDK does for the sync tools.
            state = await anyio.to_thread.run_sync(lambda: poker.new(opponent=opponent, seat=seat))
        except TableError as e:
            return {"error": e.message}
        if account is not None:
            # Who owns this table session, for owner-only table features
            # (oauth/provider.py, "table-session ownership").
            await provider.record_table_session(state["session"], account)
        return summarize_state(state)

    @server.tool()
    def state(ctx: Context) -> dict:
        """The current hand's state: your cards, the bot's visible
        state, the board, pot, and your legal actions. Call this if a
        result looks unexpected before acting again."""
        try:
            s = _poker_for(ctx).state()
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def act(ctx: Context, action: int) -> dict:
        """Take action `index` from the current state's `actions` list.
        Fills in which hand/step you mean from the last state this
        session saw -- you never pass those yourself."""
        try:
            s = _poker_for(ctx).act(action)
        except (NoSessionError, StaleStateError, InvalidActionError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def next_hand(ctx: Context) -> dict:
        """Deal the next hand. Only works once the current hand is over
        (state().over is true)."""
        try:
            s = _poker_for(ctx).next_hand()
        except (NoSessionError, StaleStateError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    def score(ctx: Context) -> dict:
        """hands played, net bb, bb/100, and the same split by
        opponent, computed from this session's hand history."""
        try:
            return _poker_for(ctx).score()
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}

    @server.custom_route("/version", methods=["GET"])
    async def version(request: Request) -> Response:  # noqa: ARG001
        # The git sha this process was built from, so anyone can check
        # what is running without a deploy log. Cloud Run: baked into
        # the image by the Dockerfile's OBGL_GIT_SHA build arg; local:
        # deploy/local/run.sh. "unknown" = nobody set it (a bare run).
        return JSONResponse({"sha": os.environ.get("OBGL_GIT_SHA") or "unknown"})

    return server


def main() -> None:
    base_url = os.environ.get("OBGL_TABLE_URL")
    if os.environ.get("OBGL_TRANSPORT", "stdio") == "http":
        # OAuth only exists over HTTP: stdio has no bearer tokens (its one
        # client is whoever launched the process).
        server = build_server(base_url, OAuthConfig.from_env(os.environ, MCP_PATH))
        host = os.environ.get("HOST", "0.0.0.0")
        port = int(os.environ.get("PORT", "8080"))
        server.run("streamable-http", host=host, port=port, streamable_http_path=MCP_PATH)
    else:
        build_server(base_url).run("stdio")


if __name__ == "__main__":
    main()
