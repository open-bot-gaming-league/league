// table serves a web page where a person plays heads-up no-limit hold'em
// against a blueprint: the person bets any legal size (the page offers
// the tree's menu as buttons), the blueprint plays its own tree's sizes
// through action translation (engine.TreeView), stacks reset every hand.
//
//	go run ./cmd/table -config configs/table_50bb.json -bots served.json \
//	    -hands hands.jsonl -addr 127.0.0.1:9790
//
// -bots is a JSON file listing the opponents on offer (see loadBots): for
// each its bucket file, its blueprint, and what the page says about it,
// which must include how exploitable it measured. The page uses relative
// URLs, so it works behind a proxy that strips a path prefix. API.md has
// the requests.
package main

import (
	"bufio"
	crand "crypto/rand"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// The page and anything it loads: everything under static/, served from
// the root. index.html must use relative URLs (see the package comment).
//
//go:embed static
var static embed.FS

func main() {
	config := flag.String("config", "", "betting-tree config (see configs/ for examples)")
	botsPath := flag.String("bots", "", "JSON file listing the bots to serve, the default first (required; see botFile)")
	handsPath := flag.String("hands", "", "append every finished hand to this file, one JSON object per line")
	addr := flag.String("addr", "127.0.0.1:9790", "listen address")
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	bots, closeBots, err := loadBots(*botsPath, *config)
	if err != nil {
		log.Fatal(err)
	}
	defer closeBots()
	var seed [16]byte
	if _, err := crand.Read(seed[:]); err != nil {
		log.Fatal(err)
	}
	s := newServer(bots, binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:]))
	if *handsPath != "" {
		// Sessions come back from the hands they finished, so a restart
		// (a new page, a new bot) does not cost anyone their score.
		if old, err := os.Open(*handsPath); err == nil {
			sessions, hands, skipped := s.restore(old)
			old.Close()
			log.Printf("table: %d sessions restored from %d finished hands in %s (%d lines not understood)", sessions, hands, *handsPath, skipped)
		} else if !os.IsNotExist(err) {
			log.Fatal(err)
		}
		f, err := os.OpenFile(*handsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		s.hands = f
	}
	for _, b := range bots {
		log.Printf("table: bot %q: %s (%d iterations)", b.ID, b.Blueprint, b.bp.Header.Iterations)
	}
	log.Printf("table: %s, %d bb stacks, listening on %s", *config, s.g.Config.StackBB, *addr)
	log.Fatal(http.ListenAndServe(*addr, s.handler()))
}

// botInfo is what the page says about a bot. Every field is shown as
// written.
type botInfo struct {
	ID             string `json:"id"`             // short, for requests: "cfrbr"
	Name           string `json:"name"`           // "CFR-BR, 200 buckets per street, 2,500 iterations"
	Trained        string `json:"trained"`        // when and how, one line
	Exploitability string `json:"exploitability"` // the number with its unit and how it was measured, or that it was not
	Note           string `json:"note,omitempty"`
}

// bot is an opponent on offer: a blueprint with the game it was trained
// on. The bots share the betting tree and differ in card abstraction, so
// each has its own Game (the tree indexed for its buckets).
type bot struct {
	botInfo
	Abs       string `json:"abs"`       // bucket file, or "category"
	Blueprint string `json:"blueprint"` // blueprint file
	g         *engine.Game
	bp        *engine.Blueprint
}

// loadBots reads the -bots file: a JSON array of bots, the default first.
// File names in it are relative to the file's own directory. A bot
// without a name or a stated exploitability is refused: the page must
// never offer an opponent without saying how exploitable it is.
func loadBots(path, config string) (bots []*bot, closeAll func(), err error) {
	var closers []func() error
	closeAll = func() {
		for _, c := range closers {
			c()
		}
	}
	fail := func(format string, args ...any) ([]*bot, func(), error) {
		closeAll()
		return nil, nil, fmt.Errorf("%s: "+format, append([]any{path}, args...)...)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("-bots: %w", err)
	}
	if err := json.Unmarshal(data, &bots); err != nil {
		return fail("%v", err)
	}
	if len(bots) == 0 {
		return fail("no bots")
	}
	rel := func(name string) string {
		if name == "category" || filepath.IsAbs(name) {
			return name
		}
		return filepath.Join(filepath.Dir(path), name)
	}
	seen := map[string]bool{}
	for _, b := range bots {
		if b.ID == "" || seen[b.ID] || b.Name == "" || b.Exploitability == "" {
			return fail("bot %q: id (unique), name and exploitability must all be stated", b.ID)
		}
		seen[b.ID] = true
		g, closeGame, err := engine.OpenGame("holdem", config, rel(b.Abs))
		if err != nil {
			return fail("bot %q: %v", b.ID, err)
		}
		closers = append(closers, closeGame)
		bp, err := engine.OpenBlueprint(rel(b.Blueprint), g)
		if err != nil {
			return fail("bot %q: %v", b.ID, err)
		}
		closers = append(closers, bp.Close)
		b.g, b.bp = g, bp
	}
	return bots, closeAll, nil
}

const maxSessions = 200

type server struct {
	mu   sync.Mutex
	bots []*bot // the default first
	// g is the first bot's game, for what all of them share: the betting
	// config and the card rules.
	g        *engine.Game
	rnd      *rand.Rand
	sessions map[string]*session
	hands    *os.File
}

func newServer(bots []*bot, seed1, seed2 uint64) *server {
	return &server{bots: bots, g: bots[0].g, rnd: rand.New(rand.NewPCG(seed1, seed2)), sessions: map[string]*session{}}
}

// bot finds an opponent by id; the empty id is the default.
func (s *server) bot(id string) *bot {
	for _, b := range s.bots {
		if b.ID == id || id == "" {
			return b
		}
	}
	return nil
}

// session is one person's sitting: the hand in play and the running
// score.
type session struct {
	id       string
	seatMode string // "sb", "bb" or "alternate"
	seat     int    // the person's seat in the current hand: 0 is the small blind
	opponent *bot   // in the current hand
	hand     *engine.TableHand
	view     *engine.TreeView   // the bot's view of the hand on its tree
	number   int                // of the current hand, from 1
	finished int                // hands finished
	net      int                // chips the person has won over the finished hands
	against  map[string]*[2]int // by opponent id: hands finished, chips won
	history  []handRecord
	recorded bool // the current hand is in history
	used     time.Time
}

// handRecord is a finished hand as the page's history shows it.
type handRecord struct {
	Number   int      `json:"number"`
	Opponent string   `json:"opponent"` // bot id
	Seat     string   `json:"seat"`
	You      []string `json:"you"`
	Bot      []string `json:"bot,omitempty"` // shown only after a showdown
	Board    []string `json:"board"`
	Lines    []string `json:"lines"`
	Net      int      `json:"net"`
	Outcome  string   `json:"outcome"`
	Showdown bool     `json:"showdown"`
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	fileServer := http.FileServer(http.FS(files))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // a rebuilt binary must not be met by an old page
		fileServer.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /api/info", s.api(func(*request) (any, error) { return s.gameInfo(), nil }))
	mux.HandleFunc("POST /api/new", s.api(s.newSession))
	mux.HandleFunc("GET /api/state", s.api(func(q *request) (any, error) {
		se, err := s.session(q.Session)
		if err != nil {
			return nil, err
		}
		return s.state(se, true), nil
	}))
	mux.HandleFunc("POST /api/act", s.api(s.act))
	mux.HandleFunc("POST /api/next", s.api(s.next))
	return mux
}

type request struct {
	Session string `json:"session"`
	Seat    string `json:"seat"` // new, next: "sb", "bb", "alternate"; empty keeps the session's
	// new, next: a bot id, the opponent from the hand being dealt on;
	// empty is the default bot (new) or no change (next).
	Opponent string `json:"opponent"`
	Hand     int    `json:"hand"`   // act, next: the hand the page is looking at
	Step     int    `json:"step"`   // act: the number of actions in that hand the page has seen
	Action   *int   `json:"action"` // act, the old form: index into the state's actions
	// act: a move of any legal size. Kind is fold, check, call, bet,
	// raise or allin; Amount, for a bet or raise, is the chips the seat
	// will have in this betting round ("raise to").
	Kind   string `json:"kind"`
	Amount *int   `json:"amount"`
}

// apiError is a refusal with its HTTP status.
type apiError struct {
	status int
	msg    string
}

func (e apiError) Error() string { return e.msg }

func refuse(status int, format string, args ...any) error {
	return apiError{status, fmt.Sprintf(format, args...)}
}

// api wraps a handler: decode the request (JSON body, or the query for a
// GET), run it under the lock, encode the answer.
func (s *server) api(fn func(*request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var q request
		if r.Method == http.MethodGet {
			q.Session = r.URL.Query().Get("session")
		} else if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		out, err := fn(&q)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			status := http.StatusInternalServerError
			if ae, ok := err.(apiError); ok {
				status = ae.status
			} else {
				log.Printf("table: %s: %v", r.URL.Path, err)
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(out)
	}
}

type gameInfo struct {
	Bots       []botInfo `json:"bots"` // the opponents on offer, the default first
	StackBB    int       `json:"stackBB"`
	SmallBlind int       `json:"smallBlind"`
	BigBlind   int       `json:"bigBlind"`
	Menu       string    `json:"menu"`
	Sizing     string    `json:"sizing"`
}

func (s *server) gameInfo() gameInfo {
	c := s.g.Config
	fr := func(menu []float64) string {
		parts := make([]string, len(menu))
		for i, f := range menu {
			parts[i] = fmt.Sprintf("%gx", f)
		}
		return strings.Join(parts, ", ")
	}
	infos := make([]botInfo, len(s.bots))
	for i, b := range s.bots {
		infos[i] = b.botInfo
	}
	return gameInfo{Bots: infos, StackBB: c.StackBB, SmallBlind: c.SB, BigBlind: c.BB,
		Menu: fmt.Sprintf("first bet of a round %s pot; raise %s pot; re-raises %s pot; all-in always",
			fr(c.Opening), fr(c.RaiseMenu), fr(c.ThreebetMenu)),
		Sizing: "You may bet or raise any whole number of chips from the minimum raise to all-in; the menu is the sizes the buttons offer. " +
			"The house bots bet only the menu's sizes and read other sizes as the menu sizes around them. " +
			"Their exploitability numbers are measured inside the menu, so against any size they are a lower bound."}
}

func (s *server) session(id string) (*session, error) {
	se := s.sessions[id]
	if se == nil {
		return nil, refuse(http.StatusNotFound, "no such session: start a new one")
	}
	se.used = time.Now()
	if se.hand == nil { // restored, and asked for the first time since
		if err := s.deal(se); err != nil {
			return nil, err
		}
	}
	return se, nil
}

// restore rebuilds sessions from a hands file: each session's score and
// history are its finished hands, so that is all it takes. The hand that
// was in play when the old server stopped is gone; its number is left
// unused, so a click the page had ready for it is refused as stale
// instead of landing on the new hand's cards. A restored session is dealt
// its next hand when it is first asked for, against the bot it last
// played if that is still on offer.
func (s *server) restore(r io.Reader) (sessions, hands, skipped int) {
	last := map[string]string{} // session -> opponent id of its last hand
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		var line handLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil || line.Session == "" || line.Number < 1 {
			skipped++
			continue
		}
		se := s.sessions[line.Session]
		if se == nil {
			se = &session{id: line.Session, against: map[string]*[2]int{}}
			s.sessions[se.id] = se
		}
		se.seatMode = line.SeatMode
		if se.seatMode != "sb" && se.seatMode != "bb" {
			se.seatMode = "alternate"
		}
		se.history = append(se.history, line.handRecord)
		se.finished++
		se.net += line.Net
		tally := se.against[line.Opponent]
		if tally == nil {
			tally = new([2]int)
			se.against[line.Opponent] = tally
		}
		tally[0]++
		tally[1] += line.Net
		se.number = line.Number + 1 // the number of the hand that may have been in play
		se.used, _ = time.Parse(time.RFC3339, line.Time)
		last[se.id] = line.Opponent
		hands++
	}
	for len(s.sessions) > maxSessions {
		s.dropOldest()
	}
	for id, se := range s.sessions {
		if se.opponent = s.bot(last[id]); se.opponent == nil { // a bot no longer on offer
			se.opponent = s.bots[0]
		}
	}
	return len(s.sessions), hands, skipped
}

func (s *server) dropOldest() {
	var oldest *session
	for _, se := range s.sessions {
		if oldest == nil || se.used.Before(oldest.used) {
			oldest = se
		}
	}
	delete(s.sessions, oldest.id)
}

func (s *server) newSession(q *request) (any, error) {
	if q.Seat == "" {
		q.Seat = "alternate"
	}
	if q.Seat != "sb" && q.Seat != "bb" && q.Seat != "alternate" {
		return nil, refuse(http.StatusBadRequest, "seat must be sb, bb or alternate")
	}
	opponent := s.bot(q.Opponent)
	if opponent == nil {
		return nil, refuse(http.StatusBadRequest, "no bot %q", q.Opponent)
	}
	if len(s.sessions) >= maxSessions {
		s.dropOldest()
	}
	var id [12]byte
	if _, err := crand.Read(id[:]); err != nil {
		return nil, err
	}
	se := &session{id: hex.EncodeToString(id[:]), seatMode: q.Seat, opponent: opponent, against: map[string]*[2]int{}, used: time.Now()}
	s.sessions[se.id] = se
	if err := s.deal(se); err != nil {
		return nil, err
	}
	return s.state(se, true), nil
}

// deal starts the session's next hand and lets the bot act if it is first.
func (s *server) deal(se *session) error {
	switch se.seatMode {
	case "sb":
		se.seat = 0
	case "bb":
		se.seat = 1
	default:
		se.seat = se.number % 2 // small blind first, then turn about
	}
	var cards [engine.TableCards]engine.Card
	for i, c := range s.rnd.Perm(s.g.Rules.Deck)[:engine.TableCards] {
		cards[i] = engine.Card(c)
	}
	hand, err := engine.NewTableHand(se.opponent.g, cards)
	if err != nil {
		return err
	}
	se.hand, se.view, se.recorded = hand, engine.NewTreeView(se.opponent.g), false
	se.number++
	return s.botActs(se)
}

// botActs plays the bot's seat until it is the person's turn or the hand
// is over, and books a finished hand.
func (s *server) botActs(se *session) error {
	h := se.hand
	for !h.Over() && h.ToAct() != se.seat {
		if err := se.view.Act(h, se.opponent.bp, s.rnd.Uint32); err != nil {
			return err
		}
	}
	if h.Over() && !se.recorded {
		se.recorded = true
		se.finished++
		se.net += h.Net(se.seat)
		tally := se.against[se.opponent.ID]
		if tally == nil {
			tally = new([2]int)
			se.against[se.opponent.ID] = tally
		}
		tally[0]++
		tally[1] += h.Net(se.seat)
		rec := s.record(se)
		se.history = append(se.history, rec)
		s.logHand(se, rec)
	}
	return nil
}

func (s *server) act(q *request) (any, error) {
	se, err := s.session(q.Session)
	if err != nil {
		return nil, err
	}
	h := se.hand
	switch {
	case q.Hand != se.number:
		return nil, refuse(http.StatusConflict, "that was hand %d; hand %d is in play", q.Hand, se.number)
	case h.Over():
		return nil, refuse(http.StatusConflict, "hand %d is over", se.number)
	case q.Step != len(h.Log):
		// A second click that was meant for an earlier decision.
		return nil, refuse(http.StatusConflict, "the hand has moved on since that was sent")
	case q.Action != nil && (q.Kind != "" || q.Amount != nil):
		return nil, refuse(http.StatusBadRequest, "send either action (a button's index) or kind and amount, not both")
	case q.Action != nil:
		if *q.Action < 0 || *q.Action >= len(h.Options()) {
			return nil, refuse(http.StatusBadRequest, "action must be one of the %d offered", len(h.Options()))
		}
		if err := h.Act(*q.Action); err != nil {
			return nil, err
		}
	default:
		kind, to, err := move(h.Legal(), q.Kind, q.Amount)
		if err != nil {
			return nil, err
		}
		if err := h.Play(kind, to); err != nil {
			return nil, refuse(http.StatusBadRequest, "%s", strings.TrimPrefix(err.Error(), "engine: "))
		}
	}
	if err := s.botActs(se); err != nil {
		return nil, err
	}
	return s.state(se, false), nil
}

// move reads an act request's kind and amount as a move: the kind for
// TableHand.Play and the "to" amount of a wager. bet and raise are the
// same move (a wager to amount; the state's legal.wager says which word
// fits); allin is a wager to legal.maxTo. An amount on a fold or check,
// or one that is not the call on a call, is refused rather than ignored:
// it means the sender misread the state. The hand checks the range.
func move(l engine.TableLegal, kind string, amount *int) (engine.ActionKind, int, error) {
	bad := func(format string, args ...any) (engine.ActionKind, int, error) {
		return 0, 0, refuse(http.StatusBadRequest, format, args...)
	}
	switch kind {
	case "fold", "check":
		if amount != nil && *amount != 0 {
			return bad("%s takes no amount", kind)
		}
		if kind == "fold" {
			return engine.ActFold, 0, nil
		}
		return engine.ActCheck, 0, nil
	case "call":
		if amount != nil && *amount != l.Call {
			return bad("a call here is %d chips, not %d", l.Call, *amount)
		}
		return engine.ActCall, 0, nil
	case "bet", "raise":
		if amount == nil {
			return bad("%s needs an amount: the chips you will have in this betting round (see legal.minTo, legal.maxTo)", kind)
		}
		return engine.ActBet, *amount, nil
	case "allin":
		if !l.Wager {
			return bad("no bet or raise is possible here: call or fold")
		}
		if amount != nil && *amount != l.MaxTo {
			return bad("all-in here is %d, not %d", l.MaxTo, *amount)
		}
		return engine.ActBet, l.MaxTo, nil
	case "":
		return bad("send action (a button's index) or kind (fold, check, call, bet, raise, allin) with amount")
	}
	return bad("kind must be fold, check, call, bet, raise or allin, not %q", kind)
}

func (s *server) next(q *request) (any, error) {
	se, err := s.session(q.Session)
	if err != nil {
		return nil, err
	}
	switch {
	case q.Hand != se.number:
		return nil, refuse(http.StatusConflict, "that was hand %d; hand %d is in play", q.Hand, se.number)
	case !se.hand.Over():
		return nil, refuse(http.StatusConflict, "hand %d is not over", se.number)
	case q.Seat != "" && q.Seat != "sb" && q.Seat != "bb" && q.Seat != "alternate":
		return nil, refuse(http.StatusBadRequest, "seat must be sb, bb or alternate")
	case q.Opponent != "" && s.bot(q.Opponent) == nil:
		return nil, refuse(http.StatusBadRequest, "no bot %q", q.Opponent)
	}
	if q.Seat != "" {
		se.seatMode = q.Seat
	}
	if q.Opponent != "" {
		se.opponent = s.bot(q.Opponent)
	}
	if err := s.deal(se); err != nil {
		return nil, err
	}
	return s.state(se, false), nil
}

// state is everything the page shows. The bot's cards are in it only
// after a showdown.
type state struct {
	Session  string   `json:"session"`
	Hand     int      `json:"hand"`
	Step     int      `json:"step"` // actions taken in the hand so far; act must send it back
	SeatMode string   `json:"seatMode"`
	Opponent string   `json:"opponent"` // the bot's id in this hand
	Seat     string   `json:"seat"`     // the person's: "small blind (button)" or "big blind"
	Round    string   `json:"round"`
	You      player   `json:"you"`
	Bot      player   `json:"bot"`
	Board    []string `json:"board"`
	Pot      int      `json:"pot"`
	Over     bool     `json:"over"`
	Actions  []button `json:"actions"`         // empty unless it is the person's turn
	Legal    *legal   `json:"legal,omitempty"` // every legal move, any size; only on the person's turn
	Lines    []string `json:"lines"`           // the hand so far, one line per action
	Events   []event  `json:"events"`          // the same actions in structured form, for the page to animate
	Outcome  string   `json:"outcome,omitempty"`
	Net      int      `json:"net"` // the person's result in this hand, once over
	Totals   totals   `json:"totals"`
	// Against is Totals split by opponent (bot id), for a session that
	// has played more than one.
	Against  map[string]totals `json:"against"`
	History  []handRecord      `json:"history,omitempty"` // every finished hand; only where the page asks for all of it
	Finished *handRecord       `json:"finished,omitempty"`
}

type player struct {
	Cards []string `json:"cards,omitempty"`
	Stack int      `json:"stack"` // chips behind
	Put   int      `json:"put"`   // chips in the pot
	Holds string   `json:"holds,omitempty"`
}

// event is one action of the hand as the page animates it.
type event struct {
	Seat  string `json:"seat"`  // "you" or "bot"
	Round int    `json:"round"` // 0 before the flop, then flop, turn, river
	Kind  string `json:"kind"`  // as a button's: fold, check, call, bet, allin
	Cost  int    `json:"cost"`  // chips the seat added
	To    int    `json:"to"`    // chips the seat has in this betting round after it, blinds included
	Text  string `json:"text"`  // the action's line in Lines: "Bot raises to 900"
}

type button struct {
	Index  int    `json:"index"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`             // fold, check, call, bet, allin
	Amount int    `json:"amount,omitempty"` // bet, allin: the "to" amount, as act's amount
}

// legal is every move open to the person, any size: what act's kind and
// amount may be.
type legal struct {
	Fold  bool `json:"fold"`
	Check bool `json:"check"`
	Call  int  `json:"call,omitempty"` // chips a call adds; absent: no call
	// Wager is "bet" or "raise", what a wager is called here; absent: no
	// wager is possible. Its amount, the chips the seat will have in this
	// betting round, is any whole number from MinTo to MaxTo; MaxTo is
	// all-in, and MinTo == MaxTo when all-in is short of a full raise.
	Wager string `json:"wager,omitempty"`
	MinTo int    `json:"minTo,omitempty"`
	MaxTo int    `json:"maxTo,omitempty"`
}

func legalOf(l engine.TableLegal) *legal {
	out := &legal{Fold: l.Fold, Check: l.Check, Call: l.Call}
	if l.Wager {
		out.Wager, out.MinTo, out.MaxTo = "bet", l.MinTo, l.MaxTo
		if l.Raise {
			out.Wager = "raise"
		}
	}
	return out
}

type totals struct {
	Hands    int     `json:"hands"`
	NetChips int     `json:"netChips"`
	NetBB    float64 `json:"netBB"`
	BBPer100 float64 `json:"bbPer100"`
}

var roundNames = [...]string{"before the flop", "flop", "turn", "river"}

func seatName(seat int) string {
	if seat == 0 {
		return "small blind (button)"
	}
	return "big blind"
}

func names(cs []engine.Card) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.String()
	}
	return out
}

func (s *server) state(se *session, all bool) state {
	h := se.hand
	bot := 1 - se.seat
	st := state{Session: se.id, Hand: se.number, Step: len(h.Log), SeatMode: se.seatMode, Opponent: se.opponent.ID, Seat: seatName(se.seat),
		Round: roundNames[h.Round()], Board: names(h.Board()), Pot: h.Put(0) + h.Put(1), Over: h.Over(),
		You:   player{Cards: names(h.Hole(se.seat)), Stack: s.g.Stack() - h.Put(se.seat), Put: h.Put(se.seat)},
		Bot:   player{Stack: s.g.Stack() - h.Put(bot), Put: h.Put(bot)},
		Lines: s.lines(se), Events: s.events(se), Actions: []button{}}
	if h.Showdown() {
		st.Round = "showdown"
		st.Bot.Cards = names(h.Hole(bot))
		st.You.Holds, st.Bot.Holds = h.HandName(se.seat), h.HandName(bot)
	}
	if h.Over() {
		st.Net, st.Outcome = h.Net(se.seat), outcome(h, se.seat)
		st.Finished = &se.history[len(se.history)-1]
	} else if h.ToAct() == se.seat {
		for i, o := range h.Options() {
			b := button{Index: i, Label: label(o, onButton), Kind: kind(o)}
			if o.Kind == engine.ActBet {
				b.Amount = o.To
			}
			st.Actions = append(st.Actions, b)
		}
		st.Legal = legalOf(h.Legal())
	}
	st.Totals = s.totals(se.finished, se.net)
	st.Against = map[string]totals{}
	for id, tally := range se.against {
		st.Against[id] = s.totals(tally[0], tally[1])
	}
	if all {
		st.History = se.history
	}
	return st
}

func (s *server) totals(hands, net int) totals {
	bb := float64(s.g.Config.BB)
	t := totals{Hands: hands, NetChips: net, NetBB: float64(net) / bb}
	if hands > 0 {
		t.BBPer100 = 100 * t.NetBB / float64(hands)
	}
	return t
}

func kind(o engine.TableOption) string {
	switch {
	case o.Kind == engine.ActFold:
		return "fold"
	case o.Kind == engine.ActCheck:
		return "check"
	case o.Kind == engine.ActCall:
		return "call"
	case o.AllIn:
		return "allin"
	}
	return "bet"
}

// The three ways an option is worded: on a button, and in the hand's log
// for the bot and for the person.
const (
	onButton = iota
	botDid
	youDid
)

var verbs = map[string][3]string{
	"fold":  {"Fold", "folds", "fold"},
	"check": {"Check", "checks", "check"},
	"call":  {"Call", "calls", "call"},
	"bet":   {"Bet", "bets", "bet"},
	"raise": {"Raise to", "raises to", "raise to"},
}

// label words an option: "Raise to 900 (1x pot)" on a button, "raises to
// 900" in the log.
func label(o engine.TableOption, form int) string {
	allIn := ""
	if o.AllIn {
		allIn = ", all-in"
	}
	switch o.Kind {
	case engine.ActFold:
		return verbs["fold"][form]
	case engine.ActCheck:
		return verbs["check"][form]
	case engine.ActCall:
		return fmt.Sprintf("%s %d%s", verbs["call"][form], o.Cost, allIn)
	}
	// A wager facing none is a bet; anything on top of chips already in
	// this round (the blinds included) is a raise.
	word := verbs["raise"][form]
	if o.Cost == o.To && int(o.Amount) == o.Cost {
		word = verbs["bet"][form]
	}
	switch {
	case form != onButton:
		return fmt.Sprintf("%s %d%s", word, o.To, allIn)
	case o.AllIn:
		return fmt.Sprintf("All-in: %s %d", strings.ToLower(word), o.To)
	}
	return fmt.Sprintf("%s %d (%.3gx pot)", word, o.To, o.PotFraction)
}

func (s *server) lines(se *session) []string {
	h := se.hand
	out := []string{}
	round := 0
	for _, a := range h.Log {
		if a.Round != round {
			round = a.Round
			out = append(out, fmt.Sprintf("-- %s: %s --", roundNames[round], strings.Join(names(h.FullBoard()[:s.g.Rules.BoardSize(round)]), " ")))
		}
		out = append(out, said(se, a))
	}
	return out
}

// said is an action as the hand's log words it: "You raise to 300", "Bot
// calls 200".
func said(se *session, a engine.TableAction) string {
	if a.Seat == se.seat {
		return "You " + label(a.TableOption, youDid)
	}
	return "Bot " + label(a.TableOption, botDid)
}

func (s *server) events(se *session) []event {
	out := make([]event, len(se.hand.Log))
	for i, a := range se.hand.Log {
		who := "bot"
		if a.Seat == se.seat {
			who = "you"
		}
		out[i] = event{Seat: who, Round: a.Round, Kind: kind(a.TableOption), Cost: a.Cost, To: a.To, Text: said(se, a)}
	}
	return out
}

func outcome(h *engine.TableHand, seat int) string {
	net := h.Net(seat)
	switch {
	case !h.Showdown() && net < 0:
		return fmt.Sprintf("You folded and lost %d", -net)
	case !h.Showdown():
		return fmt.Sprintf("The bot folded: you won %d", net)
	case net > 0:
		return fmt.Sprintf("Showdown: you won %d with %s against %s", net, h.HandName(seat), h.HandName(1-seat))
	case net < 0:
		return fmt.Sprintf("Showdown: you lost %d with %s against %s", -net, h.HandName(seat), h.HandName(1-seat))
	}
	return fmt.Sprintf("Showdown: split pot, %s each", h.HandName(seat))
}

func (s *server) record(se *session) handRecord {
	h := se.hand
	rec := handRecord{Number: se.number, Opponent: se.opponent.ID, Seat: seatName(se.seat), You: names(h.Hole(se.seat)), Board: names(h.Board()),
		Lines: s.lines(se), Net: h.Net(se.seat), Outcome: outcome(h, se.seat), Showdown: h.Showdown()}
	if h.Showdown() {
		rec.Bot = names(h.Hole(1 - se.seat))
	}
	return rec
}

// handLine is a finished hand in the hands file: its history record, with
// the session it belongs to and the cards the page never saw.
type handLine struct {
	Time     string `json:"time"`
	Session  string `json:"session"`
	SeatMode string `json:"seatMode,omitempty"`
	handRecord
	BotCards  []string `json:"botCards"`
	FullBoard []string `json:"fullBoard"`
}

// logHand appends a finished hand to the hands file.
func (s *server) logHand(se *session, rec handRecord) {
	if s.hands == nil {
		return
	}
	h := se.hand
	line, err := json.Marshal(handLine{time.Now().UTC().Format(time.RFC3339), se.id, se.seatMode, rec,
		names(h.Hole(1 - se.seat)), names(h.FullBoard())})
	if err == nil {
		_, err = s.hands.Write(append(line, '\n'))
	}
	if err != nil {
		log.Printf("table: hands file: %v", err)
	}
}
