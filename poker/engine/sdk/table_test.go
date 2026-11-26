package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine"
)

// tableServer builds and starts the real table server, cmd/table, with
// one bot (uniform play on the category abstraction), and returns its
// address.
func tableServer(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs cmd/table")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to build cmd/table with")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "table")
	if out, err := exec.Command(goBin, "build", "-o", bin, "../cmd/table").CombinedOutput(); err != nil {
		t.Fatalf("building cmd/table: %v\n%s", err, out)
	}
	g, _, err := engine.OpenHoldem(engine.TableConfig(), "category")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.NewBlueprint(g, engine.UniformStrategy{}).WriteFile(filepath.Join(dir, "uniform.bp")); err != nil {
		t.Fatal(err)
	}
	bots := `[{"id": "uniform", "name": "uniform play (test)", "note": "plays every legal move at random (test)", "abs": "category", "blueprint": "uniform.bp"}]`
	if err := os.WriteFile(filepath.Join(dir, "bots.json"), []byte(bots), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, "-config", "../configs/table_50bb.json", "-bots", filepath.Join(dir, "bots.json"), "-addr", addr)
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	base := "http://" + addr + "/"
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if res, err := http.Get(base + "health"); err == nil {
			res.Body.Close()
			return base
		}
		if time.Since(start) > 20*time.Second {
			t.Fatalf("cmd/table did not come up:\n%s", logs.String())
		}
	}
}

// The table client plays whole hands at the real table server, its
// count agrees with the table's, it picks a session up again, and it
// sends the token.
func TestTablePlays(t *testing.T) {
	base := tableServer(t)
	target, _ := url.Parse(base)
	var auth []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	defer proxy.Close()

	table := &Table{URL: proxy.URL, Token: "t0ken"}
	seen := 0
	res, err := table.Play(context.Background(), &watcher{t: t, kinds: map[Kind]int{}}, 60, 1, func(h HandResult) {
		seen++
		if h.Outcome == "" {
			t.Errorf("hand %d: no outcome", h.Hand)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hands != 60 || seen != 60 || table.Session == "" {
		t.Fatalf("%+v, %d hands reported, session %q", res, seen, table.Session)
	}
	totals := func() (hands, net int) {
		var st struct {
			Totals struct {
				Hands    int `json:"hands"`
				NetChips int `json:"netChips"`
			} `json:"totals"`
		}
		if err := table.call(context.Background(), "GET", "api/state?session="+table.Session, nil, &st); err != nil {
			t.Fatal(err)
		}
		return st.Totals.Hands, st.Totals.NetChips
	}
	if hands, net := totals(); hands != 60 || net != res.Net {
		t.Fatalf("the table counts %d hands, %d chips; the client %d, %d", hands, net, res.Hands, res.Net)
	}
	for _, a := range auth {
		if a != "Bearer t0ken" {
			t.Fatalf("a request went with Authorization %q", a)
		}
	}

	// The same session again: ten more hands, counted from here.
	more, err := table.Play(context.Background(), Random{}, 10, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hands, net := totals(); more.Hands != 10 || hands != 70 || net != res.Net+more.Net {
		t.Fatalf("picked up again: %+v; the table counts %d hands, %d chips", more, hands, net)
	}
}

func TestTableRefusals(t *testing.T) {
	base := tableServer(t)
	_, err := (&Table{URL: base}).Play(context.Background(), badBot{Action{Kind: Raise, To: 1}}, 5, 1, nil)
	var te *TableError
	if !errors.As(err, &te) || te.Status != http.StatusBadRequest || !strings.Contains(err.Error(), "refused raise to 1") {
		t.Fatalf("an illegal raise: %v", err)
	}
	_, err = (&Table{URL: base, Session: "nope"}).Play(context.Background(), Random{}, 5, 1, nil)
	if !errors.As(err, &te) || te.Status != http.StatusNotFound {
		t.Fatalf("a session the table does not know: %v", err)
	}
}

// tableState.state reads API.md's example state.
func TestTableStateExample(t *testing.T) {
	var st tableState
	example := `{"session": "9f", "hand": 12, "step": 3, "seat": "big blind", "round": "flop",
		"you": {"cards": ["As","Kd"], "stack": 4700, "put": 300, "inRound": 0},
		"bot": {"stack": 4700, "put": 300, "inRound": 0},
		"board": ["7c","9d","Kh"], "pot": 600, "over": false,
		"legal": {"fold": false, "check": true, "wager": "bet", "minTo": 100, "maxTo": 4700},
		"events": [
			{"seat": "bot", "round": 0, "kind": "bet", "cost": 250, "to": 300},
			{"seat": "you", "round": 0, "kind": "call", "cost": 200, "to": 300}]}`
	if err := json.Unmarshal([]byte(example), &st); err != nil {
		t.Fatal(err)
	}
	s, err := st.state(100, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%d %d %s %v %v %d %d %+v %+v", s.Hand, s.Seat, s.Round, s.Hole, s.Board, s.Pot, s.Stack, s.Legal, s.History)
	want := "12 1 flop [As Kd] [7c 9d Kh] 600 4700 {Fold:false Check:true Call:0 MinTo:100 MaxTo:4700} " +
		"[{Seat:0 Round:preflop Kind:raise To:300 Cost:250} {Seat:1 Round:preflop Kind:call To:300 Cost:200}]"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}
