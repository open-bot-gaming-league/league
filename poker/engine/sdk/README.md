# sdk: write a poker bot for the Open Bot Gaming League

*This page is for the coding agent building the bot. If you're the person, you don't need it: [docs/SDK.md](../../../docs/SDK.md) has the one prompt to give your coding agent.*

This directory is a Go package for writing a bot that plays the league's
poker, and running it: against other bots on your machine, or at your
table on the league's server. If you are a coding agent asked to build or
improve a bot, this page is the whole API; the code is short enough to
read after it.

The game: heads-up (two players) no-limit hold'em. Blinds 50/100 chips,
both seats start every hand with 50 big blinds (5,000 chips), any bet
size from the minimum raise to all-in.

## Prerequisites

- **Go 1.27.3 or newer** (the version `poker/engine/go.mod` names: `go version` to check).
- **git**, to clone the repository.

Don't have these? Ask your agent to install these prerequisites.

## Run it

From a clone of the repository, in `poker/engine` (the Go module's root):

    go run ./sdk/example                       # your bot vs each starter bot, 2,000 hands each
    go run ./sdk/example -vs clam -hands 20000 -seed 7
    go test ./sdk/...

`sdk/example/main.go` is the file to edit: `myBot.Act` is a bot. Copy
it, or write your own type with an `Act` method.

The starter bots are teaching bots, each with a leak to find. To see what
the example bot beats, run the matrix (`go run ./sdk/example -matrix`;
below); making it beat all three is a good first goal. Measure every
change against your previous version (below), and against the house bots
if you have their files.

## First: fork a starter bot, add one rule, measure it

The loop you will repeat: take a bot, change one thing, and `Match` the new
version against the old on the same seed. The same seed deals the same
cards, and every deal is played twice with the seats swapped, so what is
left in the result is mostly your change.

Dove never bluffs: a big bet from it means a strong hand. So, Dove with one
rule added: facing a bet of half the pot or more, fold unless the hand is
very strong (equity 0.8 or more).

```go
type respectful struct {
	bots.Dove
	strong float64 // the equity it takes to go on
}

func (b respectful) Act(s sdk.State) sdk.Action {
	if s.Legal.Call > 0 && 3*s.Legal.Call >= s.Pot && s.Equity(300) < b.strong {
		return s.CheckOrFold() // a big bet from a bot that never bluffs: let it go
	}
	return b.Dove.Act(s) // everything else: Dove as it was
}

res, _ := sdk.Match(respectful{strong: 0.8}, bots.Dove{}, 20000, 1)
fmt.Println(res) // +59.6 ± 6.0 bb/100 over 20000 hands
```

The rule wins, by nearly ten times its SE: Dove calls by price and so pays
off its own kind of bet, and the fork stops doing that. Refine it: fold a
little less (0.7).

```go
res, _ = sdk.Match(respectful{strong: 0.7}, bots.Dove{}, 20000, 1)
fmt.Println(res) // +47.1 ± 6.9 bb/100 over 20000 hands
```

Less, but the two are within noise of each other (their difference is
smaller than their SEs added up): keep the first, or run more hands to
tell them apart. Both are in `sdk/bots/example_test.go` and run with
`go test ./sdk/bots/`, which checks those exact numbers.

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
| `s.Equity(n)` | (a question) the share of pots your hand wins against one random hand, from `n` random deals (a few hundred is plenty; drawn from `s.Rand`) |

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
res, err := sdk.Match(myBot{}, bots.Clam{}, 20000, 1)
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

`bots.Dove`, `bots.Clam`, `bots.Shark` (`sdk/bots/`): three starter bots,
one short file each, and with Turtle and Ox (a download, below) the
league's five house bots. `bots.ByName["clam"]()` builds one by id; `bots.Names` has
their names.

- **Dove** (`dove.go`) never bluffs: it bets and raises in proportion to its
  equity against a random hand, calls a fair price, folds weak hands to big
  bets.
- **Clam** (`clam.go`) plays premium hands only and folds to aggression.
- **Shark** (`shark.go`) is tight and aggressive: a preflop chart by seat
  (edit it hand by hand), a continuation bet on the flop, then bets only a
  real hand.

Your own earlier versions are the other opponent worth having: keep them,
and `Match` each change against the last.

## Who beats whom: run the matrix

    go run ./sdk/cmd/matrix                    # the three starter bots, every pair
    go run ./sdk/example -matrix               # the same, with the example bot (myBot) in it
    go run ./sdk/cmd/matrix -hands 1000000     # tighter numbers (SE about 0.5 to 1), slower
    go run ./sdk/cmd/matrix -house turtle -house ox   # after downloading them (below)

Every pair plays a duplicate `Match` on the same seed (default 1, so
everyone gets the same table), and the table shows bb/100 ± SE for the row
bot against the column bot. A cell marked `~` is within 2 SE of even: too
close to call at that many hands (`HeadToHead.Close` is the same rule, for
your code); rerun with more `-hands` to separate the two. Turtle and Ox join with `-house turtle -house ox`
once downloaded. For a bot of your own, add one line to `roster` in
`sdk/cmd/matrix/main.go`, or call `sdk.Matrix` from your program with a
`sdk.Contender` for it.

## Play your table

`sdk.Table` plays your bot at your user's table on the league's server,
over the HTTP API the table page uses (`poker/engine/cmd/table/API.md`),
so they can watch it play in the browser.

1. Before anything else, add `obgl-token.txt` to your workspace's
   `.gitignore`.
2. Ask your user to sign in at https://obgl.truemirrorgames.com/table/, and
   in the side panel, under **Programs**, make the token (one per account;
   Regenerate gives a new one and stops the old). The page downloads it as
   `obgl-token.txt`: have them save it in your workspace folder. Don't ask
   them to paste it into the chat, and never print, log or commit it.
3. Run the bot, and tell them to watch the page:

       go run ./sdk/example -table -hands 50

The token is found by `sdk.FindToken`: `OBGL_TOKEN` if set, else
`obgl-token.txt` in the current folder or the nearest one above it; with
neither, the error says where to make one. `OBGL_TABLE` is the table
page's address (default `https://obgl.truemirrorgames.com/table/`); the
token is sent as `Authorization: Bearer`. In code:

```go
t, err := sdk.TableFromEnv()
if err != nil {
	log.Fatal(err) // no token: the message says where to get one
}
res, err := t.Play(ctx, myBot{}, 50, 1, func(h sdk.HandResult) { fmt.Println(h.Hand, h.Net, h.Outcome) })
```

It is your one table, the one you and your agent play too: whoever moves
first on a decision makes it, so leave it to the bot while it runs. The
bot and the seat are picked on the page. The table takes 20 requests a
second on the token (the client waits when told to), so a few hands a
second at most: for thousands of hands use `Match`. `Table` also plays a
table server you run yourself (`go run ./cmd/table`, which needs a house
bot's files; see its package comment), where it starts a session.

## Turtle and Ox: download them

The league's trained bots, Turtle and Ox, are a download (about 850 MB for
both, into a cache folder; `go run ./sdk/cmd/house where` prints it):

    go run ./sdk/cmd/house download all        # Turtle and Ox; or: download turtle
    go run ./sdk/cmd/matrix -house turtle -house ox

The files come from the league repository's `house-bots-1` release, each
checked against the sha256 in `sdk/house-bots.json`; a download picks up
where it stopped, and a file that comes down wrong is deleted and named.
NOTICE.txt, the terms the files come under, is downloaded with them: read
it. The example takes them too: `go run ./sdk/example -house turtle,ox`,
or with `-matrix`. `OBGL_HOUSE_CACHE` moves the cache.

In code, `sdk.DownloadHouse(ctx, "all")` fetches them and
`sdk.LoadHouseByID("turtle")` returns a Bot (`Close` it when done; `Clone`
plays one load in several matches at once). `sdk.LoadHouse(buckets,
blueprint)` opens files by path. A house bot plays as at the table: its
menu's bet sizes, reading other sizes as the menu sizes around them.

## Build it as WebAssembly

A bot plays in the league's own runs as a WebAssembly module, so that
every bot gets the same sandbox and the same limits. Register your bot
with `wasmbot` in an init function of its main package, and build a WASI
reactor:

    func init() { wasmbot.Register(func() (sdk.Bot, error) { return MyBot{}, nil }) }
    func main() {}

    GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o bot.wasm .

The same code still runs natively, in `Match` and your tests. In wasm the
bot's data files are read-only under `/data`, there is no network or
environment, and its clock counts its own work, not the wall: see the
`wasmbot` package comment. `poker/arena` is the runner, with the limits.

## Files

| file | what |
|---|---|
| `bot.go` | `Bot`, `State`, `Legal`, `Action`, `Event`, the helpers, `Random` |
| `cards.go` | `Card`, ranks, `Category`, `Made`, `Strength`, `Equity` |
| `match.go` | `Match`, `Result` |
| `matrix.go` | `Matrix`, `Contender`, `HeadToHead`: every pair, one table |
| `house.go` | `House`, `LoadHouse` |
| `house_download.go`, `house-bots.json` | `DownloadHouse`, `LoadHouseByID`: Turtle and Ox from the release, sha256-checked |
| `cmd/house/` | the download command |
| `table.go` | `Table`, the table client |
| `bots/` | the starter bots |
| `wasmbot/` | `Register`: your bot as a WebAssembly module; `wasmbot/abi`, the wire format |
| `example/` | a bot to start from, and the command that runs it |
| `cmd/matrix/` | the matrix command: who beats whom |

In your own Go module: `go get github.com/open-bot-gaming-league/league/poker/engine@main`
and import `github.com/open-bot-gaming-league/league/poker/engine/sdk`.
