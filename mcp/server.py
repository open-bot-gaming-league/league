"""MCP server for Bot Poker: seven tools (docs, rules, table, state, act,
next_hand, score): all but docs() wrap the live table's HTTP API, docs()
returns the documents. With sign-in (OAuth on, below) also the browser table at
/table. See
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
/docs/PRIVACY.md and /docs/TERMS.md, read from the repo files on every
request. The last two are also rendered as web pages, /docs/privacy and
/docs/terms (what the sign-in screens link to). A client that cannot reach
GitHub (claude.ai's fetcher was rate-limited there) still gets them
from here, or from the docs() tool.

Over HTTP with OBGL_PUBLIC_URL set, OAuth is on (oauth/__init__.py):
this server is the connector's OAuth authorization server, /mcp needs a
bearer token, and /login is the sign-in page (GitHub or Google).
Each poker session then belongs to the signed-in account as well as to
its connection (see `_poker_for`). Unset: no auth, as before.

With OAuth on, the same process also serves the browser table at /table
(table_web.py: the Go table's own page, proxied, with sign-in), and
tables.py gives each account one table: every connection of the account's
agent plays it, as do the user in the browser and the user's programs
(programs.py), and whoever moves first on a decision makes it. table()
and score() return the table's address for the user; a move someone else
made first comes back from act() and next_hand() as the new state with a
note, and the bot and seat are the user's to pick on the page.

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
from markdown_lite import render_markdown  # noqa: E402
from pages import LEGAL_PAGES, SMALL_STYLE, legal_links, render  # noqa: E402
from session import (  # noqa: E402
    InvalidActionError,
    MovedOnError,
    NoSessionError,
    PokerSession,
    StaleStateError,
    summarize_state,
)
from table_client import TableClient, TableError  # noqa: E402
from table_web import TableWeb, register_table_routes  # noqa: E402
from programs import ProgramTokens  # noqa: E402
from tables import Party, Tables  # noqa: E402

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
    "privacy": ("PRIVACY.md", os.path.join(DOCS_DIR, "PRIVACY.md")),
    "terms": ("TERMS.md", os.path.join(DOCS_DIR, "TERMS.md")),
    "sdk": ("SDK.md", os.path.join(DOCS_DIR, "SDK.md")),
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
<li><a href="/docs/SDK.md">SDK.md</a>: play faster: script your own bot and watch it play your table</li>
</ul>
<p>Legal:</p>
<ul>
<li><a href="/docs/privacy">Privacy Policy</a> (<a href="/docs/PRIVACY.md">markdown</a>)</li>
<li><a href="/docs/terms">Terms of Service</a> (<a href="/docs/TERMS.md">markdown</a>)</li>
</ul>"""

_SIGN_IN_LINE = {
    False: "No signup.",
    True: "Claude will ask you to sign in with GitHub or Google.",
}


_TABLE_LINE = {
    False: "",
    True: '<li><a href="/table/">Your table in the browser</a> (play it, or watch your agent or your bot play it)</li>\n',
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
</ul>
<p class="small">{legal_links()}</p>""",
        SMALL_STYLE,
    )


DOCS_INDEX_HTML = render(
    "OBGL docs",
    f"""<h1>OBGL docs</h1>
<p>The documents as markdown, and the legal pages, served from the same release as the connector.</p>
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

    def _legal_route(name: str, url_path: str) -> None:
        @server.custom_route(url_path, methods=["GET"])
        async def serve(request: Request) -> Response:  # noqa: ARG001
            title, body = render_markdown(read_doc(name))
            footer = f'<p class="small"><a href="/">Home</a> &middot; {legal_links(exclude=name)}</p>'
            return HTMLResponse(render(title, f"{body}\n{footer}", SMALL_STYLE))

    for name, url_path, _label in LEGAL_PAGES:
        _legal_route(name, url_path)


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
    # account (the access token's subject: "github:<id>" or
    # "google:<sub>"), and the key says so: "<account>/<mcp-session-id>".
    # The SDK already refuses a request whose token's (client, issuer,
    # subject) differs from the one that opened the mcp session
    # (streamable_http_manager.py: "Session not found"), so this is
    # ownership made explicit, not the only guard.
    poker_by_key: dict[str, PokerSession] = {}  # leaks on idle/disconnect; see deploy/cloudrun/CLOUDRUN.md

    def _account() -> str | None:
        """The signed-in account calling this tool ("github:<id>",
        "google:<sub>"), from the request's access token;
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
        and, as an agent, who is at the table (tables.py presence)."""
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
            "docs('agents') first, then rules(), then table(): the one tool for your "
            "table (it replaces new_session, which is gone). "
            "docs() also has 'readme', 'ways-to-play', 'sdk' and 'rules'. "
            "If your user asks to play faster, or whether this costs a lot of tokens, "
            "the next step is to script their own poker bot with the SDK: tell them so, "
            "check the prerequisites first (Go 1.27.3+ and git; their coding agent can "
            "install them), then give them docs('sdk')'s three steps (a program token from "
            "the table page, `go run ./sdk/example -table`, then watch the bot play on the page)."
            + (
                " table() is your user's one table, and returns its table_url: give it to "
                "the user, who sees it in the browser (signed in as the same account) and "
                "can play it too; whoever moves first on a decision makes it. Your user "
                "picks the bot and the seat on that page."
                if oauth
                else ""
            )
        ),
    )
    _register_docs_routes(server, oauth=oauth is not None)
    # With sign-in: each account's one table and who is at it (tables.py),
    # program tokens (programs.py), and the browser table at /table
    # (table_web.py) on top of them.
    tables = Tables(oauth.store, client) if oauth else None
    web = TableWeb(provider, tables, ProgramTokens(oauth.store), client, oauth.public_url, oauth.resource_url) if oauth else None
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
        first), 'readme', 'ways-to-play', 'sdk' (when your user wants to
        play faster or spend fewer tokens: script their own bot with the
        Go SDK, three steps), 'privacy', 'terms', or 'rules' (the static
        rules text; rules() adds the live bet menu and opponents)."""
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

    def _table_fields() -> dict:
        return {"table_url": f"{oauth.public_url.rstrip('/')}/table/"}

    async def _on_table(ctx: Context, poker: PokerSession) -> str | None:
        """With sign-in: this connection plays the account's one table.
        Attach to it the first time, and again if it changed (the user reset
        it on the page), so a move never goes to an old one. Returns a note
        for the agent when it changed under it."""
        if tables is None:
            return None
        account = _account()
        session_id = await tables.current(account)
        if session_id is None or session_id != poker.session_id:
            # First use on this connection, or a table made or reset since.
            session_id, state = await tables.ensure(account)
            known = poker.session_id is not None
            await anyio.to_thread.run_sync(lambda: poker.attach(session_id))
            tables.observe(session_id, state)
            tables.seen(session_id, Party.agent(_connection_key(ctx)))
            return "your user reset the table on the page (a new score); this is its state now" if known else None
        tables.seen(session_id, Party.agent(_connection_key(ctx)))
        return None

    async def _agent_step(ctx: Context, step: Callable[[PokerSession], dict], *, deal: bool) -> dict:
        """act / next_hand on the table, made under its lock. A move someone
        else made first (the user at the page, their program) comes back as
        the new state with a note saying so, not as an error."""
        poker = _poker_for(ctx)
        if tables is None:
            return summarize_state(await anyio.to_thread.run_sync(lambda: step(poker)))
        changed = await _on_table(ctx, poker)
        if changed:
            return {"note": changed + ": decide again from it", **summarize_state(poker.last_state)}
        session_id, before = poker.session_id, dict(poker.last_state or {})
        try:
            async with tables.lock(session_id):
                state = await anyio.to_thread.run_sync(lambda: step(poker))
                tables.moved(session_id, Party.agent(_connection_key(ctx)), {**before, "over": deal or before.get("over")})
        except MovedOnError:
            state = await anyio.to_thread.run_sync(poker.state)
            tables.observe(session_id, state)
            who = tables.last_mover(session_id) or "someone else at the table"
            what = "dealt the next hand" if deal else "acted on this decision"
            return {"note": f"{who} already {what}; this is the new state. "
                            "Act on it if it is your turn (legal is there when it is).",
                    **summarize_state(state)}
        tables.observe(session_id, state)
        return summarize_state(state)

    @server.tool()
    async def table(ctx: Context, opponent: str | None = None, seat: str | None = None) -> dict:
        """Your table, as it stands: the hand in play and, on your turn,
        `legal`. Call this first, then act() and next_hand(); it is the one
        way to your table. With sign-in it is
        your user's one table, which they also see at `table_url`: whoever
        moves first on a decision makes it (your user in the browser, you,
        or a program of theirs), and they pick the bot and the seat there;
        an opponent or seat other than the table's is refused. Without
        sign-in it starts this connection's session the first time, with
        opponent (a bot id from rules()) and seat ("sb", "bb" or
        "alternate", the default) if given."""
        poker = _poker_for(ctx)
        if tables is None:
            # No sign-in, no account and no page: each connection has its own
            # session, started the first time, with the settings asked for.
            try:
                if poker.session_id is None:
                    state = await anyio.to_thread.run_sync(lambda: poker.new(opponent=opponent, seat=seat))
                else:
                    state = await anyio.to_thread.run_sync(poker.state)
            except TableError as e:
                return {"error": e.message}
            where = "set when this connection's session started"
        else:
            try:
                await _on_table(ctx, poker)
                state = await anyio.to_thread.run_sync(poker.state)
            except TableError as e:
                return {"error": e.message}
            where = f"changed on the table page, by your user (at {_table_fields()['table_url']})"
        changes = [f"{k} {v!r}" for k, v, now in (("opponent", opponent, state["opponent"]), ("seat", seat, state["seatMode"]))
                   if v not in (None, "") and v != now]
        if changes:
            return {"error": f"settings are {where}: this table plays opponent {state['opponent']!r}, "
                             f"seat {state['seatMode']!r} (you asked for {', '.join(changes)}). "
                             "Call table() with no settings to play it as it is."}
        out = {**summarize_state(state), "seatMode": state["seatMode"]}
        return {**out, **_table_fields()} if tables is not None else out


    @server.tool()
    async def state(ctx: Context) -> dict:
        """The current hand's state: your cards, the bot's visible
        state, the board, pot, and your legal actions. Call this if a
        result looks unexpected before acting again."""
        poker = _poker_for(ctx)
        try:
            note = await _on_table(ctx, poker)
            s = await anyio.to_thread.run_sync(poker.state)
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        if tables is not None:
            tables.observe(poker.session_id, s)
        out = summarize_state(s)
        return {"note": note, **out} if note else out

    @server.tool()
    async def act(ctx: Context, action: int | str, amount: int | None = None) -> dict:
        """Make your move. action: "fold", "check", "call", "bet",
        "raise" or "allin"; amount, for a bet or raise: any whole number
        of chips from the state's legal.minTo to legal.maxTo, as a
        "raise to" amount (the chips you will have in this betting round,
        blinds included). Also accepted: action = an `index`
        from the state's `actions` list, no amount. Fills in which
        hand/step you mean from the last state this session saw -- you
        never pass those yourself. If someone else at the table moved
        first, the answer is the new state with a `note` saying so."""
        try:
            return await _agent_step(ctx, lambda poker: poker.act(action, amount), deal=False)
        except (NoSessionError, StaleStateError, InvalidActionError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}

    @server.tool()
    async def next_hand(ctx: Context) -> dict:
        """Deal the next hand. Only works once the current hand is over
        (state().over is true)."""
        try:
            return await _agent_step(ctx, lambda poker: poker.next_hand(), deal=True)
        except (NoSessionError, StaleStateError) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}

    @server.tool()
    async def score(ctx: Context) -> dict:
        """hands played, net bb, bb/100, and the same split by
        opponent, computed from this table's hand history."""
        poker = _poker_for(ctx)
        try:
            await _on_table(ctx, poker)
            out = await anyio.to_thread.run_sync(poker.score)
        except (NoSessionError,) as e:
            return {"error": str(e)}
        except TableError as e:
            return {"error": e.message}
        if tables is not None:
            out.update(_table_fields())
        return out

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
