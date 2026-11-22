"""MCP server for Bot Poker: seven tools (docs, rules, new_session, state,
act, next_hand, score): six wrap the live table's HTTP API, docs() returns
the documents. With sign-in (OAuth on, below) two more, my_sessions and
claim_session, and the browser table at /table. See
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

With OAuth on, the same process also serves the browser table at /table
(table_web.py: the Go table's own page, proxied, with sign-in, Play and
Watch), and tables.py records each table session's owner and who is
playing it: the browser, or the agent on one connection. act() and
next_hand() run only for the session's controller; new_session() and
score() return the owner's watch link; my_sessions() and claim_session()
let the owner's agent pick up a session the owner hands it (or plays in
the browser). The controller changes, the pending decision does not.

OBGL_TABLE_URL points at the table; defaults to
http://localhost:9777/poker/ for a local non-container run. Inside the
deploy/cloudrun/ container the entrypoint sets it to the table's own
in-container address.
"""

from __future__ import annotations

import os
import sys
from collections.abc import Callable

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
from table_web import TableWeb, register_table_routes  # noqa: E402
from tables import BROWSER, NotInControl, Party, TableSessions, is_controller, is_guest  # noqa: E402

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

Bet sizes: you may bet or raise any whole number of chips from the
minimum raise to all-in. Amounts are "raise to" amounts: the chips you
will have in this betting round, blinds included. Each state's `legal`
says what is open on your turn: fold, check, call (chips to call), and
`wager` ("bet" or "raise") with `minTo`..`maxTo` (maxTo is all-in). Act
with act("call"), act("raise", 650), act("allin"), act("check"),
act("fold"). The `actions` list is the bet menu as shortcuts (each bet
button has its `amount`); act(index) also takes one.

The house bots bet only their menu (from api/info, read live): a
round's first bet is 0.5x, 1x or 2x pot; a raise is 1x pot; a re-raise
is 1x pot; all-in. Other sizes they read as the menu sizes around them.
Their exploitability numbers are measured inside that menu, so against
any size they are a lower bound.

Each poker tool (state/act/next_hand/score) returns the same compact
shape: hand, step, round, seat, opponent, you, bot, board, pot, over,
actions, legal (your turn only), lines, and (once a hand is over)
outcome and net. `act()` fills
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


_TABLE_LINE = {
    False: "",
    True: '<li><a href="/table/">Play in your browser</a> (and watch your agent play)</li>\n',
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
{_TABLE_LINE[oauth]}<li><a href="/docs/">Docs</a> (README, AGENTS, WAYS-TO-PLAY)</li>
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


GUEST_WATCH_NOTE = (
    "No watch link for a guest: a watch link opens only for the account that "
    "started the session, and a guest is a new account at every sign-in. "
    "Connect with GitHub or Google sign-in to watch your games in the browser."
)


class NotYoursError(Exception):
    """With sign-in: this connection's table session is being played by
    someone else (the user in the browser, or another connection)."""


def _controller_for_agent(controller: dict, me: Party) -> str:
    """Who is playing a session, in words for the agent asking."""
    if controller["kind"] == BROWSER:
        return "your user, in the browser"
    if controller.get("agent") is None:
        return "nobody: your user handed it to their agent; claim_session() to play it"
    if controller["agent"] == me.conn:
        return "you"
    return "your user's agent on another connection"


def _not_yours(record: dict, session_id: str, me: Party) -> str:
    who = _controller_for_agent(record["controller"], me)
    return (
        f"you are not playing this session now (playing: {who}). state() still shows it. "
        f"To play it again, call claim_session('{session_id}') -- when your user wants you to."
    )


def build_server(base_url: str | None = None, oauth: OAuthConfig | None = None) -> MCPServer:
    client = TableClient(base_url)

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

    def _connection_key(ctx: Context) -> str:
        """This caller's per-connection key: which PokerSession is theirs,
        and, as an agent, which connection controls a table session
        (tables.py)."""
        headers = ctx.headers
        if headers is None:
            # No headers at all on this transport's request object
            # (stdio): one process serves one client, so one shared
            # session is correct -- there is nothing to key per-caller.
            return _SHARED_SESSION_KEY
        session_id = headers.get(MCP_SESSION_ID_HEADER)
        if not session_id:
            # Only expected with stateless_http=True (not used here)
            # or a non-conformant client. Fail loud rather than
            # silently fusing two callers' poker sessions.
            raise RuntimeError(
                f"no {MCP_SESSION_ID_HEADER!r} header on this request: cannot "
                "scope poker state to a connection"
            )
        account = _account()
        return f"{account}/{session_id}" if account is not None else session_id

    def _poker_for(ctx: Context) -> PokerSession:
        key = _connection_key(ctx)
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
            + (
                " new_session() returns a watch_url: give it to the user, who can watch "
                "you play in the browser (signed in as the same account) and take over. "
                "If they hand a game back to you, my_sessions() lists their games and "
                "claim_session() picks one up."
                if oauth
                else ""
            )
        ),
    )
    _register_docs_routes(server, oauth=oauth is not None)
    # With sign-in: who owns and who plays each table session (tables.py),
    # and the browser table at /table (table_web.py) on top of it.
    tables = TableSessions(oauth.store) if oauth else None
    web = TableWeb(provider, tables, client, oauth.public_url, oauth.resource_url) if oauth else None
    if provider is not None:
        register_login_routes(server, provider)
        register_table_routes(server, web)

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
            "sizing": info.get("sizing"),
            "bots": info["bots"],
            "docs": ["obgl://docs/AGENTS.md", "obgl://docs/WAYS-TO-PLAY.md"],
        }

    def _watch_fields(account: str, record: dict) -> dict:
        if is_guest(account):
            return {"watch_url": None, "watch_note": GUEST_WATCH_NOTE}
        return {"watch_url": web.watch_url(record)}

    async def _control_check(ctx: Context, poker: PokerSession) -> None:
        """With sign-in, an agent acts only in a session it controls."""
        if tables is None or poker.session_id is None:
            return  # no owners without sign-in; no session: the step itself says so
        me = Party.agent(_connection_key(ctx))
        try:
            await tables.require_control(poker.session_id, _account(), me)
        except LookupError:
            raise NotYoursError("this session is not on record for your account: call new_session()") from None
        except NotInControl as e:
            raise NotYoursError(_not_yours(e.record, poker.session_id, me)) from None

    async def _agent_step(ctx: Context, step: Callable[[PokerSession], dict]) -> dict:
        """act / next_hand: the step, if this connection controls the session
        (the check and the step under the session's lock, so a take-over
        cannot land between them)."""
        poker = _poker_for(ctx)
        if tables is None or poker.session_id is None:
            return await anyio.to_thread.run_sync(lambda: step(poker))
        session_id = poker.session_id
        async with tables.lock(session_id):
            await _control_check(ctx, poker)
            state = await anyio.to_thread.run_sync(lambda: step(poker))
        tables.observe(session_id, state)
        return state

    @server.tool()
    async def new_session(ctx: Context, opponent: str | None = None, seat: str | None = None) -> dict:
        """Start a new session and deal hand 1. opponent: a bot id from
        rules()'s `bots` (default: the first/least-exploitable). seat:
        "sb", "bb" or "alternate" (default -- alternates seat each
        hand, so neither seat's advantage dominates the score). With
        sign-in the answer has a watch_url: give it to the user, who can
        watch this session live in the browser (only they can open it)."""
        poker = _poker_for(ctx)
        account = _account()
        try:
            # The table call blocks (urllib); keep it off the event loop,
            # as the SDK does for the sync tools.
            state = await anyio.to_thread.run_sync(lambda: poker.new(opponent=opponent, seat=seat))
        except TableError as e:
            return {"error": e.message}
        out = summarize_state(state)
        if tables is not None:
            # Owned by the signed-in account, played by this connection.
            record = await tables.create(state["session"], account, Party.agent(_connection_key(ctx)))
            tables.observe(state["session"], state)
            out.update(_watch_fields(account, record))
        return out

    @server.tool()
    async def state(ctx: Context) -> dict:
        """The current hand's state: your cards, the bot's visible
        state, the board, pot, and your legal actions. Call this if a
        result looks unexpected before acting again."""
        poker = _poker_for(ctx)
        try:
            s = await anyio.to_thread.run_sync(poker.state)
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        out = summarize_state(s)
        if tables is not None:
            tables.observe(poker.session_id, s)
            try:
                await _control_check(ctx, poker)
            except NotYoursError as e:
                out["control"] = str(e)
        return out

    @server.tool()
    async def act(ctx: Context, action: int | str, amount: int | None = None) -> dict:
        """Make your move. action: "fold", "check", "call", "bet",
        "raise" or "allin"; amount, for a bet or raise: any whole number
        of chips from the state's legal.minTo to legal.maxTo, as a
        "raise to" amount (the chips you will have in this betting round,
        blinds included). Also accepted: action = an `index`
        from the state's `actions` list, no amount. Fills in which
        hand/step you mean from the last state this session saw -- you
        never pass those yourself."""
        try:
            s = await _agent_step(ctx, lambda poker: poker.act(action, amount))
        except (NoSessionError, StaleStateError, InvalidActionError, NotYoursError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    async def next_hand(ctx: Context) -> dict:
        """Deal the next hand. Only works once the current hand is over
        (state().over is true)."""
        try:
            s = await _agent_step(ctx, lambda poker: poker.next_hand())
        except (NoSessionError, StaleStateError, NotYoursError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        return summarize_state(s)

    @server.tool()
    async def score(ctx: Context) -> dict:
        """hands played, net bb, bb/100, and the same split by
        opponent, computed from this session's hand history."""
        poker = _poker_for(ctx)
        try:
            out = await anyio.to_thread.run_sync(poker.score)
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        if tables is not None:
            account = _account()
            record = await tables.table_session_for(poker.session_id, account)
            if record is not None:
                out.update(_watch_fields(account, record))
        return out

    if tables is not None:

        @server.tool()
        async def my_sessions(ctx: Context) -> dict:
            """Your user's table sessions, newest first: the ones you opened
            and the ones they play in the browser. Each says who is playing
            it now; `yours` is true for the ones you control. To play one
            (say the user handed it to you from the browser), call
            claim_session(session)."""
            account = _account()
            me = Party.agent(_connection_key(ctx))
            out = []
            for session_id, record in await tables.list_for(account):
                try:
                    s = await anyio.to_thread.run_sync(lambda sid=session_id: client.state(sid))
                except TableError:
                    continue  # the table no longer has it
                out.append(
                    {
                        "session": session_id,
                        "opponent": s["opponent"],
                        "hand": s["hand"],
                        "over": s["over"],
                        "hands_finished": s["totals"]["hands"],
                        "playing": _controller_for_agent(record["controller"], me),
                        "yours": is_controller(record, me),
                        "switches": record["switches"][-5:],
                        **_watch_fields(account, record),
                    }
                )
            return {"sessions": out}

        @server.tool()
        async def claim_session(ctx: Context, session: str) -> dict:
            """Play one of your user's sessions (from my_sessions()) from here
            on, from where it stands: the decision pending in it is yours.
            Whoever played it before (the browser, or you on another
            connection) can only watch until it is handed back. Answers
            with its state, as state() does."""
            account = _account()
            poker = _poker_for(ctx)
            if await tables.table_session_for(session, account) is None:
                return {"error": "no such session for your account: call my_sessions()"}
            try:
                async with tables.lock(session):
                    s = await anyio.to_thread.run_sync(lambda: poker.attach(session))
                    record = await tables.switch(
                        session, account, Party.agent(_connection_key(ctx)), hand=s["hand"], step=s["step"]
                    )
            except TableError as e:
                return {"error": e.message}
            tables.observe(session, s)
            return {**summarize_state(s), **_watch_fields(account, record)}

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
