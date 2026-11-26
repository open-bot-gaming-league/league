# AGENTS.md — the agent's entry point

You play **Bot Poker** (heads-up no-limit hold'em for bots) at a live
table through an MCP server. It is all play money: no gambling, ever.
Read [WAYS-TO-PLAY.md](https://obgl.truemirrorgames.com/docs/WAYS-TO-PLAY.md) ([GitHub](https://github.com/open-bot-gaming-league/league/blob/main/docs/WAYS-TO-PLAY.md)) next — it says where this
sits among the other ways to play and what is still ahead of it. If you
are connected over MCP, `docs('ways-to-play')` returns the same text.

This file has no strategy hints. The tools and the table tell you
everything you need to make legal moves.

## What this is

Bot Poker is a heads-up (you against one bot), 50 big-blind table.
Stacks **reset to 50 bb at the start of every hand** — nothing carries
over *between* hands; what accumulates across hands, and is what gets
measured, is the table's running score (see `score()`). You play
through MCP tools: all but `docs()` wrap the table's HTTP API, and
`docs()` returns these documents; you never call HTTP yourself.

## The tools

- **`docs(name)`** — returns a document as text: `readme`, `agents`
  (this file), `ways-to-play`, `sdk` (scripting a bot: see "When your
  user wants to play faster"), or `rules` (the static rules text). Call
  `docs('agents')` first if you have not read this yet.
- **`rules()`** — the bet menu, blind sizes, and the opponents on
  offer, read live from the table (so this stays correct even if the
  table's menu changes). Call this once at the start.
- **`table()`** — your table, as it stands: the hand in play and, on
  your turn, `legal`. Call it before you act. On the hosted connector
  it is your user's one table (see "Your user at the table"), and the
  answer has its `table_url`: give it to your user. Without sign-in it
  starts a session for this connection the first time.
- **`new_session(opponent?, seat?)`** — the old name of `table()`, kept
  for one release. On the hosted connector it starts nothing new: it
  returns your user's table, and refuses an `opponent` or `seat` other
  than the table's. Without sign-in it starts a new session for this
  connection, as it always has: `opponent` a bot `id` from `rules()`,
  `seat` `"sb"`, `"bb"` or `"alternate"` (the default, alternating every
  hand).
- **`state()`** — the current hand: your cards, the bot's visible
  state, the board, the pot, and on your turn `legal` (every legal
  move, any size) and `actions` (the bet menu as shortcuts). Call this if a
  result looks unexpected before acting again; it is always safe to
  call.
- **`act(action, amount?)`** — make your move: `action` is `"fold"`,
  `"check"`, `"call"`, `"bet"`, `"raise"` or `"allin"`, and `amount`,
  for a bet or raise, is any whole number of chips from `legal.minTo`
  to `legal.maxTo` (see the bet sizes below): `act("raise", 650)`,
  `act("call")`. The old form, `act(index)` with an `index` from
  `actions`, also works. You never pass a hand or step number — the
  wrapper remembers which hand/step you mean from the last state it
  saw, and rejects the call with a clear message if that is stale
  (for example if you call `act` twice for the same decision).
- **`next_hand()`** — deals the next hand. Only works once the current
  hand is over (`state().over` is `true`); otherwise you get a clear
  rejection telling you to finish the hand first.
- **`score()`** — hands played, net bb, bb/100, overall and split by
  opponent, computed from the table's hand history on the table
  (not just this connection's local count — if the table server
  restarted, your score is still correct).

### Your user at the table

On the hosted connector your user signs in with GitHub or Google, and
has **one table**: the one `table()` returns, which they also see at its
`table_url` (https://obgl.truemirrorgames.com/table/) when signed in the
same way. Everyone of theirs can play it — your user in the browser, you
(on any connection), and their own programs (a bot they wrote with the
Go SDK, using a token they make on the table page) — and **whoever moves
first on a decision makes it**:

- If someone else moved first, your `act()` or `next_hand()` does not
  happen; its answer is the table's new state with a `note` saying who
  moved ("you already acted on this decision" means your user, in the
  browser). Read it, and act again only if `legal` says it is your turn.
  Do not race your user: when they are playing, ask before you move.
- **Your user picks the bot and the seat** on the table page. You cannot
  change them: `new_session()` with other settings is refused. Ask your
  user to change them there.
- If your user resets the table (a new score) on the page, your next
  call says so in a `note` and shows the new table.

### When your user wants to play faster

If your user asks "can we play faster?", "is this costing a lot of
tokens?", or anything like it, the answer is: **the next step is to script
their own poker bot with the SDK provided**, and watch it play their table
at full speed. Tell them the three steps (`docs('sdk')` has them, with the
commands):

1. Make a program token on the table page (side panel, **Programs**).
2. On their machine: `cd poker/engine && go run ./sdk/example -table`
   in a clone of the league repository, with `OBGL_TOKEN` set to the token.
3. Watch the bot play on the table page.

Their coding agent can do the coding (the SDK is Go; a Python tutorial
they like is a short port to Go for a coding agent). You cannot run it
from here: the bot runs on their machine.

Two resources are also available if your client exposes MCP resources
for reading: `obgl://docs/AGENTS.md` (this file) and
`obgl://docs/WAYS-TO-PLAY.md`.
The same documents are on the web at https://obgl.truemirrorgames.com/docs/ (raw markdown).

## The rules of Bot Poker

- **Heads-up, 50 bb stacks, reset every hand.** Blinds: small blind 50
  chips (0.5 bb), big blind 100 chips (1 bb) — "bb" always means one
  big blind, 100 chips, unless said otherwise. Seats: "small blind
  (button)" acts first before the flop and last on every later street;
  "big blind" is the reverse.
- **Rounds**: before the flop, flop (3 cards), turn (4th card), river
  (5th card), then showdown if both of you are still in. An all-in
  before the river reveals the rest of the board at once.
- **Bet sizes**: you may bet or raise any whole number of chips from
  the minimum raise to all-in. Amounts are "raise to" amounts: the
  chips you will have in this betting round, blinds included (preflop
  the big blind already has 100 in). On your turn a state's `legal`
  says what is open: `fold`, `check` (true/false), `call` (chips a call
  adds), and `wager` (`"bet"` or `"raise"`; the two words are the same
  move here) with `minTo` and `maxTo`. `maxTo` is all-in; the minimum
  raise is the last bet or raise's size on top of the call (a big blind
  for a round's first bet), and when all-in is short of that, `minTo`
  equals `maxTo`. Read the numbers from `legal`; never guess them.
- **The house bots' bet menu** (also returned live by `rules()`, so
  trust that over this if they ever disagree): a round's first bet is
  0.5x, 1x, or 2x the pot; a raise is 1x pot; a re-raise is 1x pot;
  all-in. The bots bet only these sizes, and read any other size as
  the menu sizes around it. A state's `actions` are this menu as
  shortcuts.
- **Cards**: two characters, rank then suit — `As`, `Td`, `2c`. Ranks
  run `2 3 4 5 6 7 8 9 T J Q K A`; suits are `s h d c` (spades, hearts,
  diamonds, clubs).
- **A hand ends** by a fold, or by a showdown after the river (or
  immediately if someone is all-in and the other calls). `state()`'s
  `over` field is `true` from then until you call `next_hand()`;
  `outcome` is a one-line result and `net` is the chips you won or
  lost on that hand (negative if you lost).
- **Score** accumulates across the table's hands (on the hosted
  connector, whoever played them, until your user resets it); it does
  not compare you to anyone else's run.

## A normal turn

1. `rules()` once per session, to see the opponents on offer.
2. `table()`.
3. Loop: look at `state()`'s (or the last `act`/`next_hand` result's)
   `legal`, choose a move, call `act(action="call")` or
   `act(action="raise", amount=650)`. When `over` is `true`, call
   `next_hand()` instead.
4. `score()` whenever you want the running total.
