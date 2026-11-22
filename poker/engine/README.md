# Poker engine: one CFR engine for Kuhn, Leduc and heads-up no-limit hold'em

This is the counterfactual-regret-minimization (CFR) engine and the
browser table server behind Open Bot Gaming League's Bot Poker. The goal
is a blueprint strategy for no-limit hold'em, trained offline and served
from a memory-mapped file, strong enough to be a credible house opponent.
The engine implements Kuhn, Leduc and heads-up no-limit hold'em as three
values of one `Rules` type, so bigger card-abstraction buckets, a
different bet menu, or a different stack size are changes of input, not
of code.

## What is in the box

| piece | file | what it does |
|---|---|---|
| card rules | `rules.go`, `eval.go` | Kuhn, Leduc and hold'em as three values of one `Rules` type (deck, hole cards, board per round, showdown order) |
| betting tree | `tree.go`, `tree_nl.go`, `tree_limit.go` | no-limit tree from a JSON betting-tree config file (pot-fraction menus per wager level, minimum raise, clip to all-in, small blind's limp and option); fixed-limit tree for Kuhn and Leduc |
| card abstraction | `abstraction.go`, `holdem_abs.go`, `bucketing.go` | lossless 169 preflop classes; flop/turn/river buckets by k-means under earth mover's distance on equity histograms, precomputed to a file and mmapped |
| storage | `store.go` | regrets and strategy sums in flat arrays, element type float32, float16 or bfloat16, chosen separately for each array |
| solver | `solver.go` | external-sampling MCCFR, optionally multi-threaded |
| CFR-BR | `cfrbr.go` | sampled-subgame CFR-BR: the bucket player trained against a real-card best responder, for the least exploitable strategy the buckets can express |
| exploitability | `bestresponse.go` | exact best response on the public tree, responder sees real cards |
| inside the abstraction | `bucketed.go` | what a bucket-only responder wins: trained by the same MCCFR, measured on sampled deals |
| blueprint | `blueprint.go` | 1 byte per (infoset, action), written to a file and served through mmap |
| responders | `responder.go` | real-card players that exploit a blueprint by play, a lower bound that works on any tree: local best response (one-step lookahead) and a rollout responder |
| head-to-head | `match.go` | two blueprints on the same tree, every deal in both seatings |
| table | `table.go`, `translate.go`, `cmd/table` | a person against a blueprint in the browser: one hand at a time, integer chips, any legal bet size; the blueprint plays its own tree's menu through pseudo-harmonic action translation; session score and hand history (`cmd/table/API.md`) |

Go standard library only. Linux only (mmap through `syscall`).

## Build and test

```
go build ./...
go test ./...                 # add -short to skip the slower evaluator check
```

## Commands

Run from `poker/engine`. Anything long-running should go to the
background with its output in a file.

```
# 1. Card abstraction (once per bucket-count choice). Peak memory and run time scale with
#    bucket count; a small bucket count (e.g. 10/10/10) is fast and good for smoke-testing.
go run ./cmd/buckets -flop 10 -turn 10 -river 10 -out data/abs-10-10-10.bin

# 2. Train, with the exploitability curve and a blueprint file per checkpoint.
go run ./cmd/train -game holdem -config configs/tiny_20bb.json -abs data/abs-10-10-10.bin     -iters 1e9 -checkpoints 0,1e5,1e6,1e7,1e8     -exploit served -exploit-workers 16 -out runs/tiny > runs/tiny.tsv 2> runs/tiny.log

# 3. Exploitability of any blueprint file, later, with time per street
#    (-cpuprofile and -memprofile write pprof files).
go run ./cmd/exploit -game holdem -config configs/tiny_20bb.json -abs data/abs-10-10-10.bin     -blueprint runs/tiny/blueprint-1000000000.bp

# 4. Exploitability inside the abstraction (a lower bound, with standard error).
go run ./cmd/inside -game holdem -config configs/tiny_20bb.json -abs data/abs-10-10-10.bin     -iters 1e8 -deals 2e7 -workers 8 runs/tiny/blueprint-*.bp

# 5. On trees too big for cmd/exploit: a responder (a lower bound) and head-to-head.
go run ./cmd/respond -kind lbr -game holdem -config configs/botpoker_100bb.json -abs data/abs-10-10-10.bin     -hands 1e6 -workers 16 runs/botpoker-k10/blueprint-*.bp        # or -kind rollout
go run ./cmd/match -config configs/botpoker_100bb.json -abs-a data/abs-10-10-10.bin     -abs-b data/abs-200-200-200.bin -deals 1e7 -workers 12     runs/botpoker-k10/blueprint-100000000.bp runs/botpoker-k200/blueprint-100000000.bp

# 6. CFR-BR on a tree where the exact walk is affordable (an iteration is one sampled flop for both
#    seats, several thread-seconds on the 20 bb tree). Writes blueprint-average-N.bp and blueprint-current-N.bp.
go run ./cmd/cfrbr -game holdem -config configs/tiny_20bb.json -abs data/abs-200-200-200.bin     -iters 2e4 -checkpoints 300,1000,3000,1e4 -workers 8 -progress 500 -exploit both -out runs/cfrbr > runs/cfrbr.tsv 2> runs/cfrbr.log
#    A stopped run goes on from its last checkpoint: the same flags plus -resume runs/cfrbr/state-3000.cfrbr.
#    Whatever stops a run (a job limit, a restart) costs only the work since that checkpoint, so set the
#    checkpoints by how much you can afford to lose, and check a job limit against the run's pace.

# 7. Play against a blueprint in the browser (cmd/table/API.md has the HTTP routes the page uses).
#    -bots is a JSON file listing the opponents on offer: bucket file, blueprint, and what the page says about
#    each, which must state how exploitable it measured (cmd/table/API.md).
go run ./cmd/table -config configs/table_50bb.json -bots served.json -addr 127.0.0.1:9790

# Kuhn and Leduc go through the same commands.
go run ./cmd/train -game leduc -iters 1e7 -checkpoints 0,1e5,1e6 -exploit both
```

`cmd/buckets` flags worth knowing: `-patience N` (k-means keeps going
through N non-improving iterations; needed at high bucket counts, where
the default stops the turn clustering early), `-river-feature ochs`
(cluster river hands on their equity against 8 groups of opponent hands
instead of one equity number), `-reuse-flop-turn file` (copy those two
tables from another bucket file).

`cmd/train` flags worth knowing: `-storage` and `-sum-storage` (float32,
float16, bfloat16; the second defaults to the first), `-floor` (regret
floor; default none), `-workers` (1 is deterministic for a seed), `-exploit
served|average|both` (best-respond to the blueprint bytes, to the float
average strategy, or both). `-abs category` uses a crude built-in
abstraction that needs no file, for smoke tests.

`data/` and `runs/` are not in git: bucket files are hundreds of MB and
rebuild from the command above, bit for bit (fixed seed).

## Running the table

`cmd/table` serves the browser page in `cmd/table/static`: a person
against a blueprint, one hand at a time. The person bets any legal size;
the blueprint bets its own tree's menu and reads other sizes by action
translation (`translate.go`). `cmd/table/API.md` documents the HTTP
routes the page calls.

```
go run ./cmd/table -config configs/table_50bb.json -bots bots.json -hands hands.jsonl -addr 127.0.0.1:9790
```

Flags: `-config` (a betting-tree JSON config, e.g. `configs/table_50bb.json`),
`-bots` (required: a JSON file listing the opponents on offer, the default
first — for each, its bucket file, its blueprint, and what the page says
about it, including how exploitable it measured; see `loadBots` in
`cmd/table/main.go`), `-hands` (append every finished hand to this file, one
JSON object per line; sessions rebuild from it on restart), `-addr` (listen
address, default `127.0.0.1:9790`). The page uses relative URLs, so it
works behind a reverse proxy that strips a path prefix.

**The bucket files and blueprint files that `-bots` points to are not in
this repo.** They are large (bucket files can be hundreds of MB; blueprint
files vary with abstraction size) and reproducible: a bucket file comes
from `cmd/buckets` above with a fixed seed, and a blueprint file from
training with `cmd/train` or `cmd/cfrbr`. Build them yourself with those
commands, or get copies from wherever this project distributes them
separately — either way, they are downloaded or built outside the repo,
then named by path in the `-bots` JSON file.

## How the pieces fit

**Information sets are array offsets.** The betting tree is a flat slice of
nodes. A player's private information in a round is one bucket number. Each
decision node owns a contiguous block of `actions x buckets(round)` slots,
so the slot for (node, bucket, action) is `node.Offset + bucket*actions +
action`. Regrets, strategy sums and the blueprint's bytes are three arrays
indexed by that one number. There is no map and no per-infoset allocation
anywhere.

**Betting tree.** `tree_nl.go` builds pot-fraction bet menus per wager
level, minimum raise, clip to all-in, the small blind's limp and option,
from a JSON config file; `tree_test.go` checks the tree it builds against
an independent implementation's per-street counts for several configs.

**Card abstraction.** Preflop is the 169 lossless classes. After the flop a
hand's bucket comes from a table indexed by (suit-isomorphism class of the
board, hole cards with the same suit relabelling): 1,755 / 16,432 / 134,459
board classes times 1,326 hole slots, 2 bytes each. The features are the
standard ones: river equity against a random hand, and for flop and turn the
histogram of that equity over the cards to come; k-means with earth mover's
distance (L1 between cumulative histograms). Buckets are imperfect-recall:
the turn bucket does not remember the flop bucket.

**Solver.** External-sampling MCCFR: sample a deal, walk every action at the
traverser's nodes and one sampled action at the opponent's. With `-workers
N` the goroutines share the arrays without locks.

**Storage.** Short version: float16 regrets measure about as well as
float32 in practice. 2-byte strategy sums work but carry rounding noise;
whether that matters depends on how converged the run is. 2-byte values
are written with stochastic rounding (round to a neighbour at random,
right in expectation), because under round-to-nearest an increment
smaller than the stored value's spacing is simply lost.

**Blueprint.** One byte per infoset-action, each infoset summing to 255,
laid out exactly like the solver's arrays. `OpenBlueprint` maps the file and
refuses it if the tree or the abstraction differs from the one it was
written for. `cmd/train` maps every file it writes straight back and
compares it with the strategy in memory.

## What exploitability means here

`bestresponse.go` computes an exact best response in the real card game,
confined to the blueprint's betting tree: the responder sees its actual
cards; the strategy under test sees only buckets. For Kuhn and Leduc the
buckets are lossless, so this is the game's exploitability and goes to zero.
For bucketed hold'em it goes down to what the card abstraction costs and
stops there.

It is not a best response restricted to bucket strategies. With
imperfect-recall buckets that problem has no efficient exact algorithm; the
real-card version does, and it is the number that says how the blueprint
fares against a perfect opponent.

The two numbers part company. `cmd/inside` estimates the bucket-restricted
one from below, by training a responder that sees only buckets. That is
what the solver minimises, and it keeps falling with training. The
real-card number falls, bottoms out and rises again as the strategy fits
the buckets. So a blueprint has to be chosen by an outside measure, not by
training longest.

It does not cover bets outside the tree.

`cmd/respond` is the measure for trees too big for an exact walk. On small
trees, where both exist, LBR follows the exact number's shape but finds
only a fraction of its size against a trained blueprint: read it for
direction, not level.

Cost: the exact walk visits every suit-distinct (flop, turn, river) and
every betting node there, with a vector over the live hands at each. It
gets expensive fast as the tree grows, and is not affordable on a full
no-limit bet menu at realistic stack depths.

## Limits and what was left out

- The explicit tree is held in memory (about 40 bytes per betting
  sequence); a full no-limit bet menu at 200 bb can be millions of
  sequences, which is fine, but worth knowing before choosing a bet menu.
- Strategy sums are kept on every street; a leaner build could drop them
  on turn and river.
- k-means is plain Lloyd over all hands. At thousands of buckets on the
  turn it will need a faster assignment step or mini-batches.
- Plain MCCFR (`cmd/train`) has no save/resume of regrets, only blueprint
  output. CFR-BR has: `cmd/cfrbr` writes a state file at every checkpoint
  and `-resume` goes on from one (`cfrbr_state.go`).
- Exact best response is affordable only on small trees.
- The table (`cmd/table`) confines the person to the tree's bet sizes;
  there is no translation of off-tree bets.
