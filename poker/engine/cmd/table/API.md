# cmd/table: the HTTP API

Everything the page needs. All paths are relative to wherever the page is
served from (behind a reverse proxy that might be a subpath like `/poker/`),
so the page must call `api/...`, never `/api/...`. Files in `static/` are
embedded in the binary and served from the root; `static/index.html` is
the page.

Chips are integers; `bigBlind` chips are one bb. Seat names are "small
blind (button)" and "big blind"; the small blind acts first before the
flop and last after it. Cards are two characters, rank then suit:
`As`, `Td`, `2c`.

| request | body or query | answer |
|---|---|---|
| `GET api/info` | | `{bots: [{id, name, trained, note}], stackBB, smallBlind, bigBlind, menu, sizing}`. `bots` are the opponents on offer, the default first; there may be one. Show the current opponent's `name` and `note` (its style, as written). `menu` is the bet sizes the buttons offer; `sizing` says, in a sentence or three, that a person may bet any size and the bots play the menu (see "Bet sizes" below). |
| `POST api/new` | `{seat, opponent}`: seat `"sb"`, `"bb"` or `"alternate"` (the default); opponent a bot `id` (default: the first) | a state, with `history` (empty). Starts a session and deals hand 1. |
| `GET api/state?session=ID[&since=H]` | | a state, with the whole `history`. For a page load. 404: the server does not know the session (it restarted); start a new one. With `since`, `history` is only the hands finished after hand H, at most the latest 200, each with its `events` (as a state's): for a page replaying hands it did not see played. |
| `POST api/act` | `{session, hand, step, kind, amount}`, or the old form `{session, hand, step, action}` | a state, after your action and every bot action that followed it. `hand` and `step` are the state's own, sent back, so a second click meant for an earlier decision is refused (409). `kind` is `fold`, `check`, `call`, `bet`, `raise` or `allin`; `amount`, for `bet` and `raise`, is any whole number of chips from the state's `legal.minTo` to `legal.maxTo` (see "Bet sizes"). The old form, `action`, a button's `index`, also works; sending both forms is a 400. |
| `POST api/next` | `{session, hand, seat?, opponent?}` | a state: the next hand, dealt. Only when the current hand is over (409 otherwise). `seat` changes the seat mode and `opponent` the bot, from this hand on. |
| `GET api/results/ID` | | a ranked table's results (package league's Report): `{id, placed, summary, text}`. `placed` is the challenger's place in words ("10th of 10"), `summary` its one line ("beat 5 of 9, lost to 4, too close to call 0"), `text` the whole report as plain text for an agent to read aloud. 404: no table under that id, or the server was started without `-records`. |
| `GET results/ID` | | the same results as a page (HTML): the challenger's place and line, its result against each opponent, the places, and every pair (collapsed). |

Errors are `{error: "..."}` with status 400 (bad input), 404 (no such
session), 409 (not possible now: stale hand or step, hand over or not
over).

A state:

```
{
  "session": "9f...",          // keep it (localStorage) and send it back
  "hand": 12,                  // hand number, from 1
  "step": 3,                   // actions taken in this hand so far
  "seatMode": "alternate",
  "opponent": "cfrbr",         // the bot's id in this hand (see api/info)
  "seat": "big blind",         // yours in this hand
  "round": "flop",             // "before the flop", "flop", "turn", "river", "showdown"
  "you": {"cards": ["As","Kd"], "stack": 4700, "put": 300, "inRound": 0, "holds": "two pair"},
  "bot": {"stack": 4700, "put": 300, "inRound": 0},   // "cards" and "holds" only after a showdown;
                               // put: chips in the pot this hand; inRound: chips in this betting round, blinds included
  "board": ["7c","9d","Kh"],   // what is visible: 0, 3, 4 or 5 cards (all 5 at a showdown, also after an all-in)
  "pot": 600,                  // you.put + bot.put
  "over": false,
  "actions": [                 // the menu's buttons; empty unless it is your turn
    {"index": 0, "label": "Check", "kind": "check"},
    {"index": 1, "label": "Bet 300 (0.5x pot)", "kind": "bet", "amount": 300},
    {"index": 4, "label": "All-in: bet 4700", "kind": "allin", "amount": 4700}
  ],                           // kind: fold, check, call, bet (a bet or raise), allin; amount (bet, allin): act's amount for it
  "legal": {"fold": false, "check": true, "wager": "bet", "minTo": 100, "maxTo": 4700},
                               // only on your turn: every legal move, any size (see "Bet sizes")
  "lines": ["You raise to 300", "Bot calls 200", "-- flop: 7c 9d Kh --"],
  "events": [                  // the same actions, structured, for animation
    {"seat": "you", "round": 0, "kind": "bet", "cost": 250, "to": 300, "text": "You raise to 300"},
    {"seat": "bot", "round": 0, "kind": "call", "cost": 200, "to": 300, "text": "Bot calls 200"}
  ],                           // seat: you|bot; round 0-3; kind as a button's; cost: chips added; to: the seat's chips in that round after it; text: its line in "lines"
  "outcome": "",               // once over: "Showdown: you won 600 with two pair against a pair"
  "net": 0,                    // once over: chips you won (negative: lost)
  "totals": {"hands": 11, "netChips": -450, "netBB": -4.5, "bbPer100": -40.9},
  "against": {"cfrbr": {...}}, // totals split by opponent id; they sum to totals
  "finished": {...},           // once over: this hand's history record
  "history": [...]             // only from api/new and api/state
}
```

A history record: `{number, opponent, seat, you: [cards], bot: [cards] (showdown
only), board: [cards seen], lines: [...], net, outcome, showdown}`, plus `events`
in `finished` and in a `since` answer (the server keeps them for a session's
latest 200 hands, in memory only: the hands file never has them).

Bet sizes. A person may bet or raise any whole number of chips the rules
allow; the buttons are the bet menu's sizes (api/info `menu`) as a
shortcut. Amounts are "to" amounts: the chips the seat will have in this
betting round, blinds included, as in the log's "raise to 900". `legal`
(on your turn only):

- `fold`, `check`: whether each is allowed (fold only facing a bet).
- `call`: the chips a call adds; absent when there is nothing to call.
- `wager`: `"bet"` or `"raise"`, what a wager is called here (a raise
  when the other seat has chips in this round, the blinds included);
  absent when no wager is possible (you face an all-in, or calling puts
  you all-in). For `act`, `bet` and `raise` are the same move.
- `minTo`, `maxTo`: the wager's range. The minimum raise is the last
  bet or raise's size on top of the call (a big blind for a round's first
  wager); `maxTo` is all-in. When all-in is short of a full raise,
  `minTo == maxTo`: all-in is the only raise. An all-in short of a full
  raise does not reopen the betting (heads-up, the other seat can only
  call or fold it anyway).

`act` refuses (400), without changing the hand: an amount outside
`[minTo, maxTo]`; `bet`/`raise` without an amount; an amount on a fold or
check, or a `call` amount that is not `legal.call`; `allin` with an
amount that is not `maxTo`, or where no wager is possible; a kind the
rules do not allow now (check facing a bet, fold or call with nothing to
call).

The house bots play only the menu's sizes, on their training tree. A
bot reads any other bet as one of the two menu sizes around it (as pot
fractions of the pot after calling), at random, with the pseudo-harmonic
mapping (Ganzfried and Sandholm, IJCAI 2013); an all-in as all-in. Its
own bets are played at the same fraction of the real pot, clamped to
the legal range. If a bet it read as all-in was not one, the bot checks
or calls for the rest of the hand.

Restarts. The server rebuilds every session from the hands file when it
starts, so a session id stays good across a restart (a new page, a new
bot), with its totals and history. The hand that was in play is lost and
its number is skipped: the page's next `api/act` or `api/next` gets a 409
and should fetch `api/state`, which shows a new hand. A session's
opponent after a restart is the bot it last played, or the default if
that bot is no longer offered.

Notes for a page:

- The bot answers inside the same request, so a state may contain several
  new actions (yours, the bot's, a new street, the bot's again). To
  animate them, take `events` from the previous state's `step` on; a
  change of `round` between two events is a street dealt.
- When the bot is first to act, the state from `api/new` or `api/next`
  already has its action in `lines`.
- `you.stack + you.put` is always the starting stack; stacks reset every
  hand.
- An uncalled bet is not shown as returned: after a fold `pot` still
  includes it, and `net` is what actually changed hands.
- The page is `static/index.html` + `table.js` (the table: layout, the
  state-to-events derivation, controls) + `cardmotion.js` (the card motion
  carried over from an earlier table page of ours: spring chase, flips,
  speed-scaled clock, event queue, reduced motion) + `table.css`. Hash
  options: `#speed=2` (0.5-2), `#rm=1` (reduced motion).
- Behind the league's MCP server (`mcp/table_web.py`, at `/table/`) the
  same page gets `league` in `api/info` and the server's extra routes
  (sign-in, `api/token`; every state then carries `presence`). The table
  is then the account's one table: `session` is optional in every request
  (the account's table either way; any other id is 404), `api/new` resets
  it (a new session, the score from zero), and the user's agent (over MCP)
  and programs (with the account's program token, `Authorization: Bearer
  obgl_pt_...`; one per account, which the page makes, regenerates or revokes)
  play it too: whoever sends the move for a decision first makes it, and
  the others get 409 (the page reloads the state). Only the page may change
  the bot or the seat (`api/next` with `seat` or `opponent` from anyone else
  is 403), reset, or make, regenerate and revoke the token. `presence` is `{last: {kind,
  name, ago, hand} | null, others: [{kind, name, ago}], hands: {"<hand>":
  [names]}}`: the last move (kind `browser`, `agent` or `program`; name in
  words: "you", "your agent", "your program “my bot”"; ago in seconds), who
  else was seen lately, and who moved in each recent hand. A program over
  20 requests a second gets 429 with `Retry-After`. Without `league` (this
  server alone) it is the page described here. The extra ids it uses:
  `#presence` (who is at the table), `#league` (`#account-name`,
  `#sign-out`, and the Programs box `#programs`: with no token
  `#token-make-row` (`#token-label`, `#token-make`), with one `#token-have`
  (`#token-name`, `#token-when`, `#token-regenerate`, `#token-revoke`); a
  token just made or regenerated is downloaded as `obgl-token.txt` (the
  token and a newline, made in the page from the answer), with `#token-new`
  (`#token-download`, and `#token-show` with `#token-value`, `#token-copy`),
  `#token-error`), and
  `#replay` (shown while replaying: `input[name=rspeed]` 1x, fast, fastest;
  `#behind`; `#jump`, key J). Replaying: the page polls `api/state?since=H`
  and plays each finished hand it has not shown through the same event
  queue as a live hand, from the record's events, then the live state; it
  replays whenever a program moved in the last 30 s or more than one hand
  is waiting. `window.table` adds `handsShown` (`[{number, events,
  replay}]`: each hand shown to its end, with the moves animated),
  `behind` (hands waiting) and `replaying`.

- The page always sends the `{kind, amount}` form of `api/act`. The dock
  has three buttons: Fold, Check or Call, and the wager, `#size-go`
  (`#actions button.fold|check|call|bet`, shortcuts F, C, R), which sends
  `legal.wager` with the amount the sizer shows. The menu's own buttons
  are gone from the page (`actions` is for other clients). The sizer
  (`#sizer`: the dver is sent chips, always the amount shown. Three spots have
  their own detents and their own remembered size, `localStorage`
  `poker50.size.pre|bet|raise`, kept as a unit, never as chips: preflop
  in multiples of the bet faced (2x 2.5x 3x 4x), a postflop bet in
  fractions of the pot after calling (1/3 1/2 3/4 1, counted as
  `you.inRound + call + f * (pot + call)`, the way the menu's sizes are),
  a postflop raise in multiples of the bet faced (2.5x 3x 4x); each also
  has Min (`legal.minTo`) and All-in (`legal.maxTo`). The track spaces
  the detents evenly, is linear in chips between them, snaps within 12%
  of a gap of a detent, and moves in 0.5 bb steps otherwise. Keys: 1-6
  pick a size (no bet), arrows 0.5 bb (Shift 1 bb), PageUp/PageDown the
  next detent, R or Enter bets the size shown, F fold, C check or call,
  N next hand.
- Handed to the agent, the page shows a box with the prompt to paste
  into the agent's chat (`#codebox`, `#handoff-text`) and a Copy button
  (`#copy`; it selects the text when the clipboard is not available).
- `mcp/table_browser_check.py` (run by `mcp/test_table_browser.py`) drives the page. It relies on:
  `#actions button.<kind>` (with `data-key`, the shortcut) for the action
  buttons (fold, check, call, bet), `#next`, `#outcome`, `#stat-hands`, `#history details.hand`,
  `#bot-name`, `#opponent` (the selector, shown when
  more than one bot is on offer), the card sprites `#felt .card` with
  `data-who` (you, bot, board), `data-card` once known and class `down`
  while face down, `#you-stack` / `#bot-stack`, and `window.table`:
  `st` (the last state), `busy` (a request out or the table moving),
  `settled()` (nothing queued, every card and chip at rest), `shown`
  (the stacks as the table shows them), `chipsOnTable()`. `st` and `busy`
  are also globals. A new page either keeps those or changes the test with
  it.
