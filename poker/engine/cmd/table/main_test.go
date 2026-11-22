package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// testTable serves the 50 bb table tree with two bots on the file-free
// category abstraction: uniform play, under which every action of the
// tree gets played, and a calling station.
func testTable(t *testing.T) (*httptest.Server, *server) {
	t.Helper()
	g, closeGame, err := engine.OpenGame("holdem", "../../configs/table_50bb.json", "category")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeGame() })
	bots := []*bot{
		{botInfo: botInfo{ID: "uniform", Name: "uniform play (test)", Exploitability: "not measured (test blueprint)"},
			g: g, bp: engine.NewBlueprint(g, engine.UniformStrategy{})},
		{botInfo: botInfo{ID: "caller", Name: "calling station (test)", Exploitability: "not measured (test blueprint)"},
			g: g, bp: engine.NewBlueprint(g, engine.AlwaysCall{Tree: g.Tree})},
	}
	s := newServer(bots, 1, 2)
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts, s
}

// do sends one request and decodes the answer into out, returning the
// status.
func do(t *testing.T, ts *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if out != nil && res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
	return res.StatusCode
}

type actBody struct {
	Session string `json:"session"`
	Hand    int    `json:"hand"`
	Step    int    `json:"step"`
	Action  int    `json:"action"`
}

// The smoke test: 400 hands of random legal play over HTTP. After every
// answer the chips must add up, the buttons must be the tree's actions
// and nothing else may be accepted, and the bot's cards must stay out of
// the answer until a showdown.
func TestTableSmoke(t *testing.T) {
	ts, s := testTable(t)
	stack := s.g.Stack()
	rnd := rand.New(rand.NewPCG(3, 4))
	var st state
	if code := do(t, ts, "POST", "/api/new", map[string]string{"seat": "alternate"}, &st); code != 200 {
		t.Fatalf("new: status %d", code)
	}
	session := st.Session
	net, showdowns, folds, botFirst, kinds := 0, 0, 0, 0, map[string]int{}
	played := map[string]int{} // finished hands by opponent
	check := func() {
		t.Helper()
		se := s.sessions[session]
		h := se.hand
		if st.You.Stack+st.You.Put != stack || st.Bot.Stack+st.Bot.Put != stack || st.Pot != st.You.Put+st.Bot.Put {
			t.Fatalf("hand %d: chips do not add up: you %+v, bot %+v, pot %d", st.Hand, st.You, st.Bot, st.Pot)
		}
		if st.You.Stack < 0 || st.Bot.Stack < 0 || len(st.You.Cards) != 2 {
			t.Fatalf("hand %d: you %+v, bot %+v", st.Hand, st.You, st.Bot)
		}
		if (st.Bot.Cards != nil) != h.Showdown() {
			t.Fatalf("hand %d: bot cards %v with showdown %v", st.Hand, st.Bot.Cards, h.Showdown())
		}
		// The events replay the chips: blinds plus each seat's costs give its
		// put, and an event's "to" is its chips in that round.
		put := map[string]int{"you": s.g.Config.SB, "bot": s.g.Config.BB}
		if se.seat == 1 {
			put["you"], put["bot"] = s.g.Config.BB, s.g.Config.SB
		}
		round, inRound := 0, map[string]int{"you": put["you"], "bot": put["bot"]}
		for _, e := range st.Events {
			if e.Round != round {
				round, inRound = e.Round, map[string]int{}
			}
			put[e.Seat] += e.Cost
			inRound[e.Seat] += e.Cost
			if inRound[e.Seat] != e.To {
				t.Fatalf("hand %d: event %+v, but the seat has %d in the round", st.Hand, e, inRound[e.Seat])
			}
		}
		// The events' texts are the hand's lines, less the street markers.
		var said []string
		for _, line := range st.Lines {
			if !strings.HasPrefix(line, "--") {
				said = append(said, line)
			}
		}
		for i, e := range st.Events {
			if i >= len(said) || e.Text != said[i] || !strings.HasPrefix(e.Text, map[string]string{"you": "You ", "bot": "Bot "}[e.Seat]) {
				t.Fatalf("hand %d: event %d %+v against lines %v", st.Hand, i, e, said)
			}
		}
		if len(said) != len(st.Events) {
			t.Fatalf("hand %d: %d events for %d action lines", st.Hand, len(st.Events), len(said))
		}
		if len(st.Events) != len(h.Log) || put["you"] != st.You.Put || put["bot"] != st.Bot.Put {
			t.Fatalf("hand %d: %d events replay to %v; you put %d, bot %d", st.Hand, len(st.Events), put, st.You.Put, st.Bot.Put)
		}
		seen := map[string]bool{}
		for _, c := range append(append(append([]string{}, st.You.Cards...), st.Bot.Cards...), st.Board...) {
			if seen[c] || len(c) != 2 {
				t.Fatalf("hand %d: card %q twice or malformed", st.Hand, c)
			}
			seen[c] = true
		}
		if st.Over {
			if len(st.Actions) != 0 || st.Finished == nil || st.Finished.Number != st.Hand || st.Finished.Net != st.Net {
				t.Fatalf("hand %d over: actions %v, finished %+v, net %d", st.Hand, st.Actions, st.Finished, st.Net)
			}
			if st.Net != h.Net(se.seat) || h.Net(0)+h.Net(1) != 0 || st.Net > st.Bot.Put || -st.Net > st.You.Put {
				t.Fatalf("hand %d: net %d with %d and %d in", st.Hand, st.Net, st.You.Put, st.Bot.Put)
			}
			return
		}
		// Not over: it must be the person's turn, with exactly the tree's
		// actions on the buttons.
		opts := h.Options()
		if h.ToAct() != se.seat || len(st.Actions) != len(opts) || len(opts) < 2 {
			t.Fatalf("hand %d: seat %d to act (you are %d), %d buttons for %d options", st.Hand, h.ToAct(), se.seat, len(st.Actions), len(opts))
		}
		for i, b := range st.Actions {
			if b.Index != i || b.Kind != kind(opts[i]) || b.Label == "" || opts[i].Cost > st.You.Stack {
				t.Fatalf("hand %d: button %+v for option %+v with %d behind", st.Hand, b, opts[i], st.You.Stack)
			}
		}
	}
	check()
	for st.Totals.Hands < 400 {
		if st.Over {
			net += st.Net
			if st.Round == "showdown" {
				showdowns++
			} else {
				folds++
			}
			if st.Totals.NetChips != net || st.Totals.Hands != st.Hand {
				t.Fatalf("after hand %d: totals %+v, the hands sum to %d", st.Hand, st.Totals, net)
			}
			wantSeat := []string{"small blind (button)", "big blind"}[(st.Hand-1)%2]
			if st.Seat != wantSeat {
				t.Fatalf("hand %d alternating: seat %q, want %q", st.Hand, st.Seat, wantSeat)
			}
			if st.Finished.Opponent != st.Opponent {
				t.Fatalf("hand %d was against %q, recorded as against %q", st.Hand, st.Opponent, st.Finished.Opponent)
			}
			played[st.Opponent]++
			var sum totals
			for id, tot := range st.Against {
				if tot.Hands != played[id] {
					t.Fatalf("after hand %d: %d hands against %q, the page was told %d", st.Hand, played[id], id, tot.Hands)
				}
				sum.Hands += tot.Hands
				sum.NetChips += tot.NetChips
			}
			if sum.Hands != st.Totals.Hands || sum.NetChips != st.Totals.NetChips {
				t.Fatalf("after hand %d: the opponents' totals sum to %+v, the session's are %+v", st.Hand, sum, st.Totals)
			}
			// Every tenth hand, change opponent; otherwise it must stay.
			hand, want := st.Hand, st.Opponent
			next := map[string]any{"session": session, "hand": hand}
			if hand%10 == 0 {
				want = map[string]string{"uniform": "caller", "caller": "uniform"}[want]
				next["opponent"] = want
			}
			if code := do(t, ts, "POST", "/api/next", map[string]any{"session": session, "hand": hand, "opponent": "nobody"}, nil); code != 400 {
				t.Fatalf("next against an unknown bot: status %d", code)
			}
			st = state{}
			if code := do(t, ts, "POST", "/api/next", next, &st); code != 200 {
				t.Fatalf("next after hand %d: status %d", hand, code)
			}
			if st.Hand != hand+1 || st.Opponent != want {
				t.Fatalf("next after hand %d dealt hand %d against %q, want %q", hand, st.Hand, st.Opponent, want)
			}
			if st.Seat == "big blind" && len(st.Lines) > 0 {
				botFirst++
			}
			check()
			continue
		}
		// What must be refused, now and then: an action off the menu, a
		// stale step, the next hand before this one is over.
		if rnd.IntN(20) == 0 {
			for name, bad := range map[string]actBody{
				"past the last button": {session, st.Hand, st.Step, len(st.Actions)},
				"negative":             {session, st.Hand, st.Step, -1},
				"stale step":           {session, st.Hand, st.Step - 1, 0},
				"another hand":         {session, st.Hand + 1, st.Step, 0},
			} {
				if code := do(t, ts, "POST", "/api/act", bad, nil); code != 400 && code != 409 {
					t.Fatalf("hand %d: %s was answered with status %d", st.Hand, name, code)
				}
			}
			if code := do(t, ts, "POST", "/api/next", actBody{Session: session, Hand: st.Hand}, nil); code != 409 {
				t.Fatalf("hand %d: next in mid-hand was answered with status %d", st.Hand, code)
			}
			var again state
			do(t, ts, "GET", "/api/state?session="+session, nil, &again)
			if again.Step != st.Step || again.Pot != st.Pot || len(again.History) != st.Totals.Hands {
				t.Fatalf("hand %d: refused requests changed the hand, or the history has %d of %d hands", st.Hand, len(again.History), st.Totals.Hands)
			}
		}
		a := st.Actions[rnd.IntN(len(st.Actions))]
		kinds[a.Kind]++
		body := actBody{session, st.Hand, st.Step, a.Index}
		st = state{}
		if code := do(t, ts, "POST", "/api/act", body, &st); code != 200 {
			t.Fatalf("legal action %+v: status %d", body, code)
		}
		check()
	}
	t.Logf("400 hands: %d showdowns, %d folds, bot acted first in %d, your actions %v, opponents %v, net %d chips", showdowns, folds, botFirst, kinds, played, st.Totals.NetChips)
	if played["uniform"] < 100 || played["caller"] < 100 {
		t.Errorf("hands by opponent: %v", played)
	}
	for _, k := range []string{"fold", "check", "call", "bet", "allin"} {
		if kinds[k] == 0 {
			t.Errorf("no %s was ever played", k)
		}
	}
	if showdowns == 0 || folds == 0 || botFirst == 0 {
		t.Errorf("showdowns %d, folds %d, bot first %d", showdowns, folds, botFirst)
	}
}

type moveBody struct {
	Session string `json:"session"`
	Hand    int    `json:"hand"`
	Step    int    `json:"step"`
	Kind    string `json:"kind"`
	Amount  *int   `json:"amount,omitempty"`
}

func chips(n int) *int { return &n }

// Bets of any size over HTTP: 400 hands where the person sends kind and
// amount, often off the menu. legal is in the state exactly on the
// person's turn and is the hand's; every legal amount is accepted, and
// everything outside legal is refused without changing the hand; a
// button's kind and amount is the same move as its index; the chips add
// up and the bot (uniform play, which bets every menu size, through
// translation) never stops the hand.
func TestTableAnySize(t *testing.T) {
	ts, s := testTable(t)
	stack := s.g.Stack()
	rnd := rand.New(rand.NewPCG(5, 6))
	var st state
	if code := do(t, ts, "POST", "/api/new", map[string]string{"seat": "alternate"}, &st); code != 200 {
		t.Fatalf("new: status %d", code)
	}
	session := st.Session
	offMenu, byButton, refused := 0, 0, 0
	for st.Totals.Hands < 400 {
		se := s.sessions[session]
		h := se.hand
		if st.You.Stack+st.You.Put != stack || st.Bot.Stack+st.Bot.Put != stack || st.Pot != st.You.Put+st.Bot.Put {
			t.Fatalf("hand %d: chips do not add up: you %+v, bot %+v, pot %d", st.Hand, st.You, st.Bot, st.Pot)
		}
		if st.Over {
			if st.Legal != nil {
				t.Fatalf("hand %d is over with legal %+v", st.Hand, st.Legal)
			}
			hand := st.Hand
			st = state{}
			if code := do(t, ts, "POST", "/api/next", map[string]any{"session": session, "hand": hand}, &st); code != 200 {
				t.Fatalf("next after hand %d: status %d", hand, code)
			}
			continue
		}
		l := st.Legal
		if l == nil || *l != *legalOf(h.Legal()) || h.ToAct() != se.seat {
			t.Fatalf("hand %d: legal %+v on the person's turn, the hand's %+v", st.Hand, l, h.Legal())
		}
		if (l.Wager != "") != (l.MaxTo > 0) || l.MinTo > l.MaxTo || l.MaxTo > st.You.Put+st.You.Stack {
			t.Fatalf("hand %d: legal %+v with %d behind", st.Hand, l, st.You.Stack)
		}
		// inRound is where "raise to" counts from: a call adds what the bot is
		// ahead by, and each menu bet is to inRound + call + its pot fraction of
		// the pot after calling (how the page prices its pot-fraction presets).
		if st.You.InRound != h.InRound(se.seat) || st.Bot.InRound != h.InRound(1-se.seat) ||
			l.Call > 0 && l.Wager != "" && st.You.InRound+l.Call != st.Bot.InRound {
			t.Fatalf("hand %d: in round you %d, bot %d, call %d", st.Hand, st.You.InRound, st.Bot.InRound, l.Call)
		}
		for _, o := range h.Options() {
			want := float64(st.You.InRound+l.Call) + o.PotFraction*float64(st.Pot+l.Call)
			if o.Kind == engine.ActBet && math.Abs(float64(o.To)-want) > 0.5 {
				t.Fatalf("hand %d: menu bet to %d, pot fraction %.3f prices it at %.1f", st.Hand, o.To, o.PotFraction, want)
			}
		}
		send := func(b any) int { return do(t, ts, "POST", "/api/act", b, nil) }
		body := func(kind string, amount *int) moveBody { return moveBody{session, st.Hand, st.Step, kind, amount} }
		// Now and then, everything just outside legal, which must change nothing.
		if rnd.IntN(8) == 0 {
			bad := []moveBody{body("", nil), body("shove", nil), body("fold", chips(100)), body("bet", nil)}
			if l.Wager != "" {
				bad = append(bad, body(l.Wager, chips(l.MinTo-1)), body(l.Wager, chips(l.MaxTo+1)), body("allin", chips(l.MaxTo-1)))
			} else {
				bad = append(bad, body("allin", nil), body("raise", chips(l.MaxTo)))
			}
			if !l.Fold {
				bad = append(bad, body("fold", nil))
			}
			if !l.Check {
				bad = append(bad, body("check", nil))
			}
			if l.Call == 0 {
				bad = append(bad, body("call", nil))
			} else {
				bad = append(bad, body("call", chips(l.Call+1)))
			}
			for _, b := range bad {
				if code := send(b); code != 400 {
					t.Fatalf("hand %d: %+v (legal %+v) was answered %d", st.Hand, b, l, code)
				}
				refused++
			}
			if code := send(map[string]any{"session": session, "hand": st.Hand, "step": st.Step, "action": 0, "kind": "fold"}); code != 400 {
				t.Fatalf("hand %d: both forms at once were answered %d", st.Hand, code)
			}
			if len(h.Log) != st.Step {
				t.Fatalf("hand %d: refused moves changed the hand", st.Hand)
			}
		}
		// The move: a button by kind and amount, or any legal size, or a
		// check, call or fold.
		var b moveBody
		switch k := rnd.IntN(10); {
		case k < 3:
			btn := st.Actions[rnd.IntN(len(st.Actions))]
			b = body(btn.Kind, nil)
			if btn.Kind == "bet" {
				b.Amount = chips(btn.Amount)
			}
			byButton++
		case k < 7 && l.Wager != "":
			b = body(l.Wager, chips(l.MinTo+rnd.IntN(l.MaxTo-l.MinTo+1)))
			if rnd.IntN(2) == 0 {
				b.Kind = map[string]string{"bet": "raise", "raise": "bet"}[l.Wager] // the two words are one move
			}
			onMenu := false
			for _, btn := range st.Actions {
				onMenu = onMenu || btn.Amount == *b.Amount
			}
			if !onMenu {
				offMenu++
			}
		case l.Check:
			b = body("check", nil)
		case l.Call > 0 && rnd.IntN(3) > 0:
			b = body("call", chips(l.Call))
		case l.Fold:
			b = body("fold", nil)
		default:
			b = body("allin", nil)
		}
		step := st.Step
		st = state{}
		if code := do(t, ts, "POST", "/api/act", b, &st); code != 200 {
			t.Fatalf("legal move %+v: status %d", b, code)
		}
		mine := h.Log[step]
		if mine.Seat != se.seat || (b.Amount != nil && (b.Kind == "bet" || b.Kind == "raise") && mine.To != *b.Amount) {
			t.Fatalf("hand %d: %+v was played as %+v", st.Hand, b, mine)
		}
	}
	t.Logf("400 hands: %d moves off the menu, %d by a button's kind and amount, %d refusals", offMenu, byButton, refused)
	if offMenu < 100 || byButton < 100 {
		t.Errorf("off menu %d, by button %d", offMenu, byButton)
	}
}

// A request for a session the server does not have is a 404 (the page
// starts a new one), and the page and the bot's description are served.
func TestTableServesPageAndInfo(t *testing.T) {
	ts, _ := testTable(t)
	if code := do(t, ts, "GET", "/api/state?session=nope", nil, nil); code != 404 {
		t.Errorf("unknown session: status %d", code)
	}
	var info gameInfo
	if code := do(t, ts, "GET", "/api/info", nil, &info); code != 200 || info.StackBB != 50 || len(info.Bots) != 2 || info.Bots[0].ID != "uniform" || info.Bots[1].Exploitability == "" {
		t.Errorf("info: status %d, %+v", code, info)
	}
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), `id="exploitability"`) {
		t.Errorf("page: status %d, %d bytes", res.StatusCode, len(page))
	}
	// The page must work behind a proxy that strips a prefix.
	if strings.Contains(string(page), `"/api/`) {
		t.Error("the page calls the API by absolute path")
	}
	for _, seat := range []string{"sb", "bb"} {
		var st state
		do(t, ts, "POST", "/api/new", map[string]string{"seat": seat, "opponent": "caller"}, &st)
		want := map[string]string{"sb": "small blind (button)", "bb": "big blind"}[seat]
		if st.Seat != want || st.Opponent != "caller" {
			t.Errorf("seat %s against the caller: got %q against %q", seat, st.Seat, st.Opponent)
		}
	}
	if code := do(t, ts, "POST", "/api/new", map[string]string{"seat": "dealer"}, nil); code != 400 {
		t.Errorf("seat dealer: status %d", code)
	}
	if code := do(t, ts, "POST", "/api/new", map[string]string{"opponent": "nobody"}, nil); code != 400 {
		t.Errorf("opponent nobody: status %d", code)
	}
	var st state
	if do(t, ts, "POST", "/api/new", map[string]string{}, &st); st.Opponent != "uniform" || st.SeatMode != "alternate" {
		t.Errorf("defaults: opponent %q, seat mode %q", st.Opponent, st.SeatMode)
	}
}

// A bots file is refused unless every bot has an id of its own, a name
// and a stated exploitability: the page must never offer an opponent
// without saying how exploitable it is. File names in it are relative to
// the file.
func TestBotsFileMustStateExploitability(t *testing.T) {
	dir := t.TempDir()
	g, closeGame, err := engine.OpenGame("holdem", "../../configs/table_50bb.json", "category")
	if err != nil {
		t.Fatal(err)
	}
	defer closeGame()
	if err := engine.NewBlueprint(g, engine.UniformStrategy{}).WriteFile(dir + "/u.bp"); err != nil {
		t.Fatal(err)
	}
	const good = `{"id":"a","name":"x","exploitability":"0.5 bb/hand, exact","abs":"category","blueprint":"u.bp"}`
	for body, ok := range map[string]bool{
		`[` + good + `]`: true,
		`[` + good + `,` + strings.Replace(good, `"id":"a"`, `"id":"b"`, 1) + `]`: true,
		`[` + good + `,` + good + `]`: false, // the same id twice
		`[` + strings.Replace(good, `"exploitability":"0.5 bb/hand, exact",`, ``, 1) + `]`: false,
		`[` + strings.Replace(good, `"name":"x",`, ``, 1) + `]`:                            false,
		`[` + strings.Replace(good, `u.bp`, `missing.bp`, 1) + `]`:                         false,
		`[]`: false,
	} {
		path := dir + "/bots.json"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		bots, closeBots, err := loadBots(path, "../../configs/table_50bb.json")
		if (err == nil) != ok {
			t.Errorf("%s: err %v, want ok %v", body, err, ok)
		}
		if err == nil {
			if bots[0].ID != "a" || bots[0].bp == nil {
				t.Errorf("%s: first bot %+v", body, bots[0].botInfo)
			}
			closeBots()
		}
	}
}

// Sessions come back from the hands file after a restart: score, split by
// opponent, and history as they were; the hand that was in play is gone
// and a click prepared for it is refused, not applied to the new cards.
func TestSessionsSurviveARestart(t *testing.T) {
	ts, s := testTable(t)
	path := t.TempDir() + "/hands.jsonl"
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s.hands = f

	// Twelve hands as the big blind, checking or calling, the opponent
	// changed half way; then one action into hand 13 and stop.
	var st state
	do(t, ts, "POST", "/api/new", map[string]string{"seat": "bb"}, &st)
	session := st.Session
	passive := func() int {
		for _, b := range st.Actions {
			if b.Kind == "check" || b.Kind == "call" {
				return b.Index
			}
		}
		return 0
	}
	for st.Totals.Hands < 12 {
		if st.Over {
			next := map[string]any{"session": session, "hand": st.Hand}
			if st.Hand == 6 {
				next["opponent"] = "caller"
			}
			do(t, ts, "POST", "/api/next", next, &st)
			continue
		}
		body := actBody{session, st.Hand, st.Step, passive()}
		st = state{}
		do(t, ts, "POST", "/api/act", body, &st)
	}
	var before state
	do(t, ts, "GET", "/api/state?session="+session, nil, &before)
	do(t, ts, "POST", "/api/next", actBody{Session: session, Hand: before.Hand}, &st)
	inPlay := actBody{session, st.Hand, st.Step, passive()} // a click the page has ready
	if st.Hand != 13 || st.Over {
		t.Fatalf("hand %d, over %v", st.Hand, st.Over)
	}

	// The restart: a new server with the same bots reads the file.
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	s2 := newServer(s.bots, 5, 6)
	if sessions, hands, skipped := s2.restore(old); sessions != 1 || hands != 12 || skipped != 0 {
		t.Fatalf("restored %d sessions from %d hands, %d lines skipped", sessions, hands, skipped)
	}
	ts2 := httptest.NewServer(s2.handler())
	defer ts2.Close()

	if code := do(t, ts2, "POST", "/api/act", inPlay, nil); code != 409 {
		t.Errorf("a click meant for the lost hand: status %d, want 409", code)
	}
	var after state
	if code := do(t, ts2, "GET", "/api/state?session="+session, nil, &after); code != 200 {
		t.Fatalf("state after the restart: status %d", code)
	}
	if after.Totals != before.Totals || len(after.Against) != 2 || after.Against["uniform"] != before.Against["uniform"] || after.Against["caller"] != before.Against["caller"] {
		t.Errorf("totals after %+v %+v, before %+v %+v", after.Totals, after.Against, before.Totals, before.Against)
	}
	if len(after.History) != 12 || after.History[11].Outcome != before.History[11].Outcome || len(after.History[3].Lines) != len(before.History[3].Lines) {
		t.Errorf("history after the restart has %d hands", len(after.History))
	}
	if after.Hand != 14 || after.Over || after.Opponent != "caller" || after.SeatMode != "bb" || after.Seat != "big blind" {
		t.Errorf("after the restart: hand %d, over %v, against %q, seat mode %q", after.Hand, after.Over, after.Opponent, after.SeatMode)
	}
	// And play goes on, counted on top of what was there.
	st = after
	for !st.Over {
		body := actBody{session, st.Hand, st.Step, passive()}
		st = state{}
		if code := do(t, ts2, "POST", "/api/act", body, &st); code != 200 {
			t.Fatalf("play after the restart: status %d", code)
		}
	}
	if st.Totals.Hands != 13 || st.Totals.NetChips != before.Totals.NetChips+st.Net {
		t.Errorf("after one more hand: totals %+v, were %+v, the hand won %d", st.Totals, before.Totals, st.Net)
	}
	if code := do(t, ts2, "GET", "/api/state?session=someone-else", nil, nil); code != 404 {
		t.Errorf("an unknown session after the restart: status %d", code)
	}
}
