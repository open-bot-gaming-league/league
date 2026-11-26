# sdk: write a poker bot for the Open Bot Gaming League

This directory is a Go package for writing a bot that plays the league's
poker, and running it: against other bots on your machine, or at your
table on the league's server. If you are a coding agent asked to build or
improve a bot, this page is the whole API; the code is short enough to
read after it.

The game: heads-up (two players) no-limit hold'em. Blinds 50/100 chips,
both seats start every hand with 50 big blinds (5,000 chips), any bet
size from the minimum raise to all-in.

## Run it

From a clone of the repository, in `poker/engine` (the Go module's root;
Go 1.27.3 or newer, as go.mod says):

    go run ./sdk/example                       # your bot vs each starter bot, 2,000 hands each
    go run ./sdk/example -vs rock -hands 20000 -seed 7
    go test ./sdk/...

`sdk/example/main.go` is the file to edit: `myBot.Act` is a bot. Copy
it, or write your own type with an `Act` method.

The starter bots are teaching bots, each with one obvious leak, so even
the example bot beats them by a lot (well over +100 bb/100 against
Station and Maniac). That says little about a bot: measure yours against
a house bot, or against your previous version.

## A bot

```go
type Bot interface {
	Act(s sdk.State) sdk.Action
}
```

`Act` is called each time your seat must move, with what your seat can
see, and returns your move. Return only legal moves; the helpers below
build nothing else.

```go
type myBot struct{}

func (myBot) Act(s sdk.State) sdk.Action {
	if s.Made() >= sdk.Pair {
		return s.RaisePot(0.5) // bet or raise half the pot
	}
	return s.CheckOrFold()
}
```

### State

| field | meaning |
|---|---|
| `Hand` | hand number, from 1; a new number is a new hand |
| `Seat` | `sdk.SmallBlind` (the button: acts first before the flop, last after) or `sdk.BigBlind` |
| `Round` | `sdk.Preflop`, `Flop`, `Turn`, `River` |
| `Hole` | your two cards |
| `Board` | the cards dealt so far: 0, 3, 4 or 5 |
| `Pot` | chips both seats have put in this hand, blinds and unanswered bets included |
| `Stack`, `OppStack` | chips behind (not yet put in) |
| `InRound`, `OppInRound` | chips put in during this betting round, blinds included |
| `Legal` | the moves open to you now (below) |
| `History` | this hand's moves so far, oldest first: `Event{Seat, Round, Kind, To, Cost}` |
| `BigBlind` | chips in a big blind (100) |
| `Rand` | your random source; use it, not a global, so a seed replays a game |

`Legal`: `Fold`, `Check` (bools); `Call`, the chips a call adds (0: no
call); `MinTo`..`MaxTo`, the range of a bet or raise (0 when none is
possible). `Legal.CanRaise()` says whether one is.

### Action

`sdk.Action{Kind, To}`. `Kind` is `sdk.Fold`, `Check`, `Call` or `Raise`
(a bet and a raise are the same move). For `Raise`, `To` is the chips
you will have put in **during this betting round, blinds included**: "raise
to 300" from the small blind before the flop is `To: 300` (it costs 250
more). Betting is open: any whole number from `Legal.MinTo` to `Legal.MaxTo` is
legal, with no bet menu; `MaxTo` is all-in. (The house bots keep a menu
of their own inside and read your sizes onto it; that limits them, not you.)

Helpers on State, each always legal:

| helper | move |
|---|---|
| `s.CheckOrCall()` | check if you can, else call |
| `s.CheckOrFold()` | check if you can, else fold |
| `s.RaiseTo(n)` | bet or raise to n, moved into the legal range; checks or calls if no raise is possible |
| `s.RaisePot(f)` | bet or raise by f times the pot after calling: 0.5 is half pot, 1 is pot (the table's own sizing unit) |
| `s.AllIn()` | raise to `MaxTo`, or call when you cannot raise |
| `s.Facing()` | (a question) has the other seat bet or raised this round since you last moved? |
| `s.Made()` | your best hand's category now: `sdk.HighCard` .. `sdk.StraightFlush` (preflop: `Pair` or `HighCard`) |

Cards: `sdk.Card`, with `c.Rank()` 0..12 (`sdk.Two` .. `sdk.Ace`),
`c.Suit()` 0..3, `c.String()` like `"As"`. `sdk.ParseCard("Td")` reads one;
`sdk.Strength(hole, board)` compares two hands on a board (higher wins).

### The rules you need

- The small blind posts 50, the big blind 100. The small blind acts first
  before the flop and last on the flop, turn and river.
- A minimum raise is the size of the last bet or raise on top of the call
  (a big blind for a round's first bet). All-in short of a full raise is
  allowed (then `MinTo == MaxTo`).
- Stacks reset to 5,000 every hand. Results are counted in big blinds.

You do not need to enforce these: locally every move goes through the
engine's `TableHand` (`poker/engine/table.go`), the code the league's
table server runs, and at the table the server checks it. An illegal move
stops a match with an error that names the hand.

## Measure it: Match

```go
res, err := sdk.Match(myBot{}, bots.Rock{}, 20000, 1)
fmt.Println(res) // like "+17.4 ± 2.5 bb/100 over 20000 hands"
```

`Match(a, b, hands, seed)` plays every deal twice, seats swapped, so each
bot plays both sides of the same cards and most of the luck cancels. It
reports `a`'s win rate in big blinds per 100 hands with its standard
error. A difference smaller than about two SEs may be noise; four times
the hands halves the SE. The same seed plays the same match exactly.
Thousands of hands take well under a second.

`sdk.Random{}` plays random legal moves of any size: a quick way to send
your bot into every kind of spot (a match fails on an illegal move).

## Opponents

- `bots.Station`, `bots.Maniac`, `bots.Rock` (`sdk/bots/`): three starter
  bots, one short file each. Station calls almost everything, Maniac
  raises and shoves with anything, Rock plays only premium hands and folds
  to aggression. `bots.ByName["rock"]()` builds one by name.
- House bots: the league's trained bots, if their files are on your disk
  (a bucket file and a blueprint, hundreds of MB each; not in the
  repository, and nothing here downloads them):

      go run ./sdk/example -house-buckets abs-200-200-200.bin -house-blueprint turtle-k200-1500.bp

  In code, `sdk.LoadHouse(buckets, blueprint)` returns a Bot; `Close` it
  when done. It plays as at the table: its menu's bet sizes, reading other
  sizes as the menu sizes around them.

## Play your table

`sdk.Table` plays your bot at your table on the league's server, over
the HTTP API the table page uses (`poker/engine/cmd/table/API.md`), so you
can watch it play in the browser.

1. Sign in at https://obgl.truemirrorgames.com/table/ (GitHub or Google).
2. In the side panel, under **Programs**, make a token. It is shown once:
   copy it. Anyone with it can play your table; revoke it there when done.
3. Run your bot with it, and watch the page:

       export OBGL_TOKEN=obgl_pt_...
       go run ./sdk/example -table -hands 50

`OBGL_TABLE` is the table page's address (default
`https://obgl.truemirrorgames.com/table/`); the token is sent as
`Authorization: Bearer`. In code:

```go
t := sdk.TableFromEnv()
res, err := t.Play(ctx, myBot{}, 50, 1, func(h sdk.HandResult) { fmt.Println(h.Hand, h.Net, h.Outcome) })
```

It is your one table, the one you and your agent play too: whoever moves
first on a decision makes it, so leave it to the bot while it runs. The
bot and the seat are picked on the page. The table takes 20 requests a
second per token (the client waits when told to), so a few hands a
second at most: for thousands of hands use `Match`. `Table` also plays a
table server you run yourself (`go run ./cmd/table`, which needs a house
bot's files; see its package comment), where it starts a session.

## Files

| file | what |
|---|---|
| `bot.go` | `Bot`, `State`, `Legal`, `Action`, `Event`, the helpers, `Random` |
| `cards.go` | `Card`, ranks, `Category`, `Made`, `Strength` |
| `match.go` | `Match`, `Result` |
| `house.go` | `House`, `LoadHouse` |
| `table.go` | `Table`, the table client |
| `bots/` | the starter bots |
| `example/` | a bot to start from, and the command that runs it |

In your own Go module: `go get github.com/open-bot-gaming-league/league/poker/engine@main`
and import `github.com/open-bot-gaming-league/league/poker/engine/sdk`.
