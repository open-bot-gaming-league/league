package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LeagueTable is the league's table: the page at this address, and the
// API under it.
const LeagueTable = "https://obgl.truemirrorgames.com/table/"

// Table plays a bot at a table over the table's HTTP API
// (poker/engine/cmd/table/API.md): the requests the table page makes,
// so the page shows the bot's hands as it plays them.
type Table struct {
	// URL is the table page's address, ending in "/"; the API is under it
	// (URL + "api/act"). A table you run yourself, `go run ./cmd/table`,
	// is "http://127.0.0.1:9790/".
	URL string
	// Token, when set, is sent as "Authorization: Bearer <Token>": a
	// program token, made on the league's table page (Programs). It plays
	// your account's table as you.
	Token string
	// Session is the table session to play. Empty: at the league, your
	// account's one table; at a table you run, a new session. Play sets it.
	Session string
	// Opponent is the table bot's id for a new session at a table you run
	// (see api/info); empty is the table's default. At the league you pick
	// the bot on the table page.
	Opponent string
	// HTTP is the client requests go through; nil is one with a 30 s
	// timeout.
	HTTP *http.Client
}

// TokenFile is the name of the file a program token is saved in: the
// table page's Programs box downloads it, and the user saves it in their
// coding agent's workspace folder.
const TokenFile = "obgl-token.txt"

// TableFromEnv is the table named by the environment: OBGL_TABLE (the
// page's address; default LeagueTable), with the program token from
// FindToken. At the league a token is required: with none it is an error
// that says where to make one. A table you run yourself (OBGL_TABLE set)
// may need none.
func TableFromEnv() (*Table, error) {
	t := &Table{URL: os.Getenv("OBGL_TABLE")}
	token, err := FindToken()
	if t.URL == "" {
		t.URL = LeagueTable
		if err != nil {
			return nil, err
		}
	}
	t.Token = token
	return t, nil
}

// FindToken finds the program token: the OBGL_TOKEN environment variable
// if it is set, else the file obgl-token.txt (TokenFile) in the current
// directory or the nearest parent that has one. An error, naming both and
// where to make a token, when there is neither.
func FindToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("OBGL_TOKEN")); token != "" {
		return token, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, TokenFile))
		if err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				return token, nil
			}
			return "", fmt.Errorf("sdk: %s is empty", filepath.Join(dir, TokenFile))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("sdk: no program token: set OBGL_TOKEN, or save " + TokenFile +
		" (the table page, " + LeagueTable + ", side panel, Programs: Make a token downloads it) in this folder or one above it")
}

// HandResult is one finished hand at the table.
type HandResult struct {
	Hand    int
	Net     int    // chips your bot won (negative: lost)
	Outcome string // the table's words: "Showdown: you won 600 with two pair against a pair"
}

// Play plays bot at the table for `hands` finished hands, calling
// onHand (if not nil) after each, and reports how the bot did over them.
// The table deals; seed seeds State.Rand. A move the table refuses ends
// the run with the table's reason. SE is over single hands: a table
// does not deal each deal twice, so it is far wider than a Match's.
func (t *Table) Play(ctx context.Context, bot Bot, hands int, seed uint64, onHand func(HandResult)) (Result, error) {
	var info struct {
		BigBlind int             `json:"bigBlind"`
		League   json.RawMessage `json:"league"` // only at the league's table
	}
	if err := t.call(ctx, "GET", "api/info", nil, &info); err != nil {
		return Result{}, err
	}
	if info.BigBlind <= 0 {
		return Result{}, fmt.Errorf("sdk: %sapi/info gave no bigBlind", t.base())
	}
	var st tableState
	var err error
	switch {
	case t.Session == "" && info.League != nil:
		// The league: your account's one table, as it stands (its bot and
		// seat are picked on the table page).
		st, err = t.step(ctx, "GET", "api/state?brief=1", nil)
		if err == nil && st.Over {
			st, err = t.step(ctx, "POST", "api/next", map[string]any{"hand": st.Hand})
		}
	case t.Session == "":
		st, err = t.step(ctx, "POST", "api/new", map[string]any{"seat": "alternate", "opponent": t.Opponent})
	default:
		// A session picked up between hands: its last hand was someone
		// else's to count.
		st, err = t.step(ctx, "GET", "api/state?brief=1&session="+url.QueryEscape(t.Session), nil)
		if err == nil && st.Over {
			st, err = t.step(ctx, "POST", "api/next", map[string]any{"session": t.Session, "hand": st.Hand})
		}
	}
	if err != nil {
		return Result{}, err
	}
	t.Session = st.Session
	r := rand.New(rand.NewPCG(seed, 1))
	var res Result
	var sumSq float64
	stale := 0
	for res.Hands < hands {
		switch {
		case st.Over:
			res.Hands++
			res.Net += st.Net
			x := float64(st.Net) / float64(info.BigBlind)
			sumSq += x * x
			if onHand != nil {
				onHand(HandResult{Hand: st.Hand, Net: st.Net, Outcome: st.Outcome})
			}
			if res.Hands < hands {
				st, err = t.step(ctx, "POST", "api/next", map[string]any{"session": t.Session, "hand": st.Hand})
			}
		case st.Legal == nil:
			return res, fmt.Errorf("sdk: hand %d is not over and it is not your turn: is someone else playing this session?", st.Hand)
		default:
			var s State
			if s, err = st.state(info.BigBlind, r); err != nil {
				return res, err
			}
			var a Action
			if a, err = ask(bot, s); err != nil {
				return res, fmt.Errorf("sdk: hand %d, %s (%T): %w", st.Hand, s.Round, bot, err)
			}
			body := map[string]any{"session": t.Session, "hand": st.Hand, "step": st.Step, "kind": a.Kind.String()}
			if a.Kind == Raise {
				body["amount"] = a.To
			}
			if st, err = t.step(ctx, "POST", "api/act", body); err != nil && !isStale(err) {
				return res, fmt.Errorf("sdk: hand %d, %s: the table refused %v: %w", s.Hand, s.Round, a, err)
			}
		}
		// A 409: the hand moved on without us (the server restarted, or
		// the page played). Look again, a few times at most.
		if isStale(err) && stale < 3 {
			stale++
			st, err = t.step(ctx, "GET", "api/state?brief=1&session="+url.QueryEscape(t.Session), nil)
		} else if err == nil {
			stale = 0
		}
		if err != nil {
			return res, err
		}
	}
	if res.Hands > 0 {
		n := float64(res.Hands)
		mean := float64(res.Net) / float64(info.BigBlind) / n
		res.BBPer100 = 100 * mean
		if n > 1 {
			res.SE = 100 * math.Sqrt(max((sumSq-n*mean*mean)/(n-1), 0)/n)
		}
	}
	return res, nil
}

// TableError is a request the table answered with an error.
type TableError struct {
	Status  int
	Message string
}

func (e *TableError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return fmt.Sprintf("%d %s (the table needs your account: make a program token on the table page, "+
			"under Programs, and set OBGL_TOKEN to it)", e.Status, e.Message)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Message)
}

func isStale(err error) bool {
	var te *TableError
	return errors.As(err, &te) && te.Status == http.StatusConflict
}

func (t *Table) base() string {
	if strings.HasSuffix(t.URL, "/") {
		return t.URL
	}
	return t.URL + "/"
}

// step is a request whose answer is a state: always a fresh one (a
// field the table leaves out must not keep its old value).
func (t *Table) step(ctx context.Context, method, path string, body any) (tableState, error) {
	var st tableState
	err := t.call(ctx, method, path, body, &st)
	return st, err
}

// call makes one API request and decodes the answer into out.
func (t *Table) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	client := t.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, data, err := send(client, req)
	if err == nil && res.StatusCode == http.StatusTooManyRequests {
		// Over the table's rate for a program: wait as long as it says, once.
		wait, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		select {
		case <-time.After(time.Duration(max(wait, 1)) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		if req.GetBody != nil {
			req.Body, _ = req.GetBody()
		}
		res, data, err = send(client, req)
	}
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &TableError{Status: res.StatusCode, Message: e.Error}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("sdk: %s %s: %w", method, path, err)
	}
	return nil
}

func send(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	return res, data, err
}

// tableState is the part of the table's state (API.md) a bot needs.
type tableState struct {
	Session string `json:"session"`
	Hand    int    `json:"hand"`
	Step    int    `json:"step"`
	Seat    string `json:"seat"`
	Round   string `json:"round"`
	You     struct {
		Cards   []string `json:"cards"`
		Stack   int      `json:"stack"`
		InRound int      `json:"inRound"`
	} `json:"you"`
	Bot struct {
		Stack   int `json:"stack"`
		InRound int `json:"inRound"`
	} `json:"bot"`
	Board []string `json:"board"`
	Pot   int      `json:"pot"`
	Over  bool     `json:"over"`
	Legal *struct {
		Fold  bool   `json:"fold"`
		Check bool   `json:"check"`
		Call  int    `json:"call"`
		Wager string `json:"wager"`
		MinTo int    `json:"minTo"`
		MaxTo int    `json:"maxTo"`
	} `json:"legal"`
	Events []struct {
		Seat  string `json:"seat"`
		Round int    `json:"round"`
		Kind  string `json:"kind"`
		Cost  int    `json:"cost"`
		To    int    `json:"to"`
	} `json:"events"`
	Outcome string `json:"outcome"`
	Net     int    `json:"net"`
}

var tableRounds = map[string]Round{"before the flop": Preflop, "flop": Flop, "turn": Turn, "river": River}
var tableKinds = map[string]Kind{"fold": Fold, "check": Check, "call": Call, "bet": Raise, "allin": Raise}

// state is the table's state as the bot's State.
func (st *tableState) state(bigBlind int, r *rand.Rand) (State, error) {
	bad := func(format string, args ...any) (State, error) {
		return State{}, fmt.Errorf("sdk: hand %d: the table's state: "+format, append([]any{st.Hand}, args...)...)
	}
	s := State{
		Hand: st.Hand, Pot: st.Pot, Stack: st.You.Stack, OppStack: st.Bot.Stack,
		InRound: st.You.InRound, OppInRound: st.Bot.InRound, BigBlind: bigBlind, Rand: r,
		Legal: Legal{Fold: st.Legal.Fold, Check: st.Legal.Check, Call: st.Legal.Call},
	}
	switch st.Seat {
	case "small blind (button)":
		s.Seat = SmallBlind
	case "big blind":
		s.Seat = BigBlind
	default:
		return bad("seat %q", st.Seat)
	}
	round, ok := tableRounds[st.Round]
	if !ok {
		return bad("round %q", st.Round)
	}
	s.Round = round
	if st.Legal.Wager != "" {
		s.Legal.MinTo, s.Legal.MaxTo = st.Legal.MinTo, st.Legal.MaxTo
	}
	if len(st.You.Cards) != 2 {
		return bad("%d hole cards", len(st.You.Cards))
	}
	for i, name := range st.You.Cards {
		c, err := ParseCard(name)
		if err != nil {
			return bad("%v", err)
		}
		s.Hole[i] = c
	}
	for _, name := range st.Board {
		c, err := ParseCard(name)
		if err != nil {
			return bad("%v", err)
		}
		s.Board = append(s.Board, c)
	}
	for _, e := range st.Events {
		kind, ok := tableKinds[e.Kind]
		if !ok {
			return bad("move kind %q", e.Kind)
		}
		seat := s.Seat
		if e.Seat != "you" {
			seat = 1 - s.Seat
		}
		s.History = append(s.History, Event{Seat: seat, Round: Round(e.Round), Kind: kind, To: e.To, Cost: e.Cost})
	}
	return s, nil
}
