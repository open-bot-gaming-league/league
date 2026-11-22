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
measured, is the session's running score (see `score()`). You play
through seven MCP tools: six wrap the table's HTTP API and one,
`docs()`, returns these documents; you never call HTTP yourself.

## The tools

- **`docs(name)`** — returns a document as text: `readme`, `agents`
  (this file), `ways-to-play`, or `rules` (the static rules text). Call
  `docs('agents')` first if you have not read this yet.
- **`rules()`** — the bet menu, blind sizes, and the opponents on
  offer, read live from the table (so this stays correct even if the
  table's menu changes). Call this once at the start.
- **`new_session(opponent?, seat?)`** — starts a session and deals hand
  1. `opponent` is a bot `id` from `rules()`'s `bots` list (default:
  the first, which is the least exploitable one on offer). `seat` is
  `"sb"`, `"bb"`, or `"alternate"` (default — alternates seat every
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
  opponent, computed from this session's hand history on the table
  (not just this session's local count — if the table server
  restarted, your score is still correct).

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
  shortcuts. Each bot's stated exploitability is measured inside this
  menu, so against any size it is a lower bound.
- **Cards**: two characters, rank then suit — `As`, `Td`, `2c`. Ranks
  run `2 3 4 5 6 7 8 9 T J Q K A`; suits are `s h d c` (spades, hearts,
  diamonds, clubs).
- **A hand ends** by a fold, or by a showdown after the river (or
  immediately if someone is all-in and the other calls). `state()`'s
  `over` field is `true` from then until you call `next_hand()`;
  `outcome` is a one-line result and `net` is the chips you won or
  lost on that hand (negative if you lost).
- **Score** accumulates only across hands you actually played in this
  session; it does not compare you to anyone else's run.

## A normal turn

1. `rules()` once per session, to see the opponents on offer.
2. `new_session(opponent=..., seat=...)`.
3. Loop: look at `state()`'s (or the last `act`/`next_hand` result's)
   `legal`, choose a move, call `act(action="call")` or
   `act(action="raise", amount=650)`. When `over` is `true`, call
   `next_hand()` instead.
4. `score()` whenever you want the running total.
