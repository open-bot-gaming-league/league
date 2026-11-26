package sdk

import (
	"errors"
	"fmt"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// House is one of the league's own bots: a trained blueprint and the
// bucket file it was trained with, the two files the table server loads
// for it. It plays exactly as it does at the table: the menu's bet sizes
// on its own betting tree, reading other sizes as the menu sizes around
// them (engine.TreeView).
//
// The files are large (hundreds of MB) and are not part of the
// repository; LoadHouse only opens files already on disk.
type House struct {
	g     *engine.Game
	bp    *engine.Blueprint
	close func() error

	// By seat (one House may play both): the hand each view follows,
	// and where that hand is on the bot's tree.
	hand [2]int
	view [2]*engine.TreeView
}

// LoadHouse opens a house bot from its bucket file and blueprint. The
// blueprint must have been trained on the league table's betting config
// with that bucket file; a mismatch is an error. buckets may be
// "category" for a blueprint trained on the file-free category
// abstraction (tests use it). Close it when done.
func LoadHouse(buckets, blueprint string) (*House, error) {
	g, closeGame, err := engine.OpenHoldem(engine.TableConfig(), buckets)
	if err != nil {
		return nil, err
	}
	bp, err := engine.OpenBlueprint(blueprint, g)
	if err != nil {
		closeGame()
		return nil, err
	}
	return &House{g: g, bp: bp, close: func() error { return errors.Join(bp.Close(), closeGame()) }}, nil
}

// Close releases the files.
func (b *House) Close() error { return b.close() }

// Act rebuilds the hand as the seat sees it (the cards it cannot see
// are stand-ins, which the bot never looks at), lets the bot's view of
// it make the move, and returns that move. The view lives for the
// whole hand, so a bet it read one way stays read that way.
func (b *House) Act(s State) Action {
	if b.view[s.Seat] == nil || s.Hand != b.hand[s.Seat] {
		b.hand[s.Seat], b.view[s.Seat] = s.Hand, engine.NewTreeView(b.g)
	}
	h, err := b.mirror(s)
	if err == nil {
		err = b.view[s.Seat].Act(h, b.bp, s.Rand.Uint32)
	}
	if err != nil {
		panic(fmt.Sprintf("house bot: %v", err)) // Match reports it as this bot's error
	}
	last := h.Log[len(h.Log)-1]
	return Action{Kind: kindOf(last.Kind), To: last.To}
}

// mirror is a TableHand with s's cards and s's moves.
func (b *House) mirror(s State) (*engine.TableHand, error) {
	var cards [engine.TableCards]engine.Card
	var known [engine.TableCards]bool
	var used uint64
	put := func(i int, c Card) {
		cards[i], known[i] = c, true
		used |= 1 << c
	}
	put(2*s.Seat, s.Hole[0])
	put(2*s.Seat+1, s.Hole[1])
	for i, c := range s.Board {
		put(4+i, c)
	}
	next := Card(0)
	for i := range cards {
		for ; !known[i]; next++ {
			if used&(1<<next) == 0 {
				put(i, next)
			}
		}
	}
	h, err := engine.NewTableHand(b.g, cards)
	if err != nil {
		return nil, err
	}
	for _, e := range s.History {
		if e.Seat != h.ToAct() {
			return nil, fmt.Errorf("the history has seat %d acting where seat %d was to act", e.Seat, h.ToAct())
		}
		if err := apply(h, Action{Kind: e.Kind, To: e.To}); err != nil {
			return nil, err
		}
	}
	if h.ToAct() != s.Seat {
		return nil, fmt.Errorf("asked to act for seat %d where seat %d is to act", s.Seat, h.ToAct())
	}
	return h, nil
}
