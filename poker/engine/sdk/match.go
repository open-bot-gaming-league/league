package sdk

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// Result is how a match went for its first bot.
type Result struct {
	Hands    int     // hands played
	Net      int     // chips the first bot won (negative: lost)
	BBPer100 float64 // the first bot's win rate: big blinds won per 100 hands
	// SE is the standard error of BBPer100: about two in three matches
	// with other seeds land within one SE of the true rate. Smaller with
	// more hands (a quarter of it at 16 times the hands).
	SE float64
}

func (r Result) String() string {
	return fmt.Sprintf("%+.1f ± %.1f bb/100 over %d hands", r.BBPer100, r.SE, r.Hands)
}

// game is the league table's game: its rules, from the engine.
func game() (*engine.Game, error) {
	return engine.NewHoldem(engine.TableConfig(), engine.CategoryAbstraction{Ranks: 13})
}

// Match plays a against b on the league table's rules and reports how a
// did. Every deal is played twice with the seats swapped, each bot
// getting the other's cards the second time, so the luck of the cards
// mostly cancels; hands is rounded up to an even number. The same seed
// plays the same match: it picks the cards and seeds each bot's
// State.Rand. A move that is not legal, or a bot that panics, ends the
// match with an error naming the hand.
func Match(a, b Bot, hands int, seed uint64) (Result, error) {
	g, err := game()
	if err != nil {
		return Result{}, err
	}
	deck := rand.New(rand.NewPCG(seed, 0))
	rands := [2]*rand.Rand{rand.New(rand.NewPCG(seed, 1)), rand.New(rand.NewPCG(seed, 2))}
	deals := (hands + 1) / 2
	bb := float64(g.Config.BB)
	var res Result
	var sum, sumSq float64 // of a's net per deal, in big blinds
	for d := range deals {
		var cards [engine.TableCards]engine.Card
		for i, c := range deck.Perm(g.Rules.Deck)[:engine.TableCards] {
			cards[i] = engine.Card(c)
		}
		// a in the small blind, then a in the big blind with the same cards.
		first, err := playHand(g, cards, [2]Bot{a, b}, [2]*rand.Rand{rands[0], rands[1]}, 2*d+1)
		if err != nil {
			return res, err
		}
		second, err := playHand(g, cards, [2]Bot{b, a}, [2]*rand.Rand{rands[1], rands[0]}, 2*d+2)
		if err != nil {
			return res, err
		}
		net := first - second // second is b's net in seat 0
		res.Net += net
		x := float64(net) / bb
		sum, sumSq = sum+x, sumSq+x*x
	}
	res.Hands = 2 * deals
	if deals > 0 {
		mean := sum / float64(deals)
		res.BBPer100 = 50 * mean // a deal is two hands
		if deals > 1 {
			variance := (sumSq - float64(deals)*mean*mean) / float64(deals-1)
			res.SE = 50 * math.Sqrt(max(variance, 0)/float64(deals))
		}
	}
	return res, nil
}

// playHand plays one hand between seats[0] (the small blind) and
// seats[1], and returns seat 0's net.
func playHand(g *engine.Game, cards [engine.TableCards]engine.Card, seats [2]Bot, rands [2]*rand.Rand, number int) (int, error) {
	h, err := engine.NewTableHand(g, cards)
	if err != nil {
		return 0, err
	}
	for !h.Over() {
		seat := h.ToAct()
		s := stateOf(g, h, seat, number, rands[seat])
		a, err := ask(seats[seat], s)
		if err == nil {
			err = apply(h, a)
		}
		if err != nil {
			return 0, fmt.Errorf("sdk: hand %d, %s, seat %d (%T): %w", number, s.Round, seat, seats[seat], err)
		}
	}
	return h.Net(0), nil
}

// ask calls a bot, turning a panic into an error.
func ask(b Bot, s State) (a Action, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the bot panicked: %v", p)
		}
	}()
	return b.Act(s), nil
}

// apply plays a move on the hand; the engine refuses an illegal one.
func apply(h *engine.TableHand, a Action) error {
	kind, err := engineKind(a.Kind)
	if err != nil {
		return err
	}
	if err := h.Play(kind, a.To); err != nil {
		return fmt.Errorf("%v: %w", a, err)
	}
	return nil
}

// stateOf is what seat sees of the hand.
func stateOf(g *engine.Game, h *engine.TableHand, seat, number int, r *rand.Rand) State {
	opp := 1 - seat
	l := h.Legal()
	s := State{
		Hand: number, Seat: seat, Round: Round(h.Round()),
		Board: append([]Card(nil), h.Board()...),
		Pot:   h.Pot(), Stack: g.Stack() - h.Put(seat), OppStack: g.Stack() - h.Put(opp),
		InRound: h.InRound(seat), OppInRound: h.InRound(opp),
		Legal:    Legal{Fold: l.Fold, Check: l.Check, Call: l.Call},
		History:  make([]Event, len(h.Log)),
		BigBlind: g.Config.BB, Rand: r,
	}
	copy(s.Hole[:], h.Hole(seat))
	if l.Wager {
		s.Legal.MinTo, s.Legal.MaxTo = l.MinTo, l.MaxTo
	}
	for i, a := range h.Log {
		s.History[i] = Event{Seat: a.Seat, Round: Round(a.Round), Kind: kindOf(a.Kind), To: a.To, Cost: a.Cost}
	}
	return s
}
