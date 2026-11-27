package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"

	"github.com/open-bot-gaming-league/league/poker/engine/league"
)

// The results page: one ranked table's report (league.Report), from its
// record. The page is self-contained (its style inline) and uses relative
// URLs, like the table's page.
//
//go:embed results.html
var resultsHTML string

var resultsPage = template.Must(template.New("results").Funcs(template.FuncMap{
	"result":       league.ResultText,
	"thousands":    league.Thousands,
	"sign":         signClass,
	"verdictClass": verdictClass,
}).Parse(resultsHTML))

// signClass is the page's colour for a number: pos, neg, or none for 0.
func signClass(x float64) string {
	switch {
	case x > 0:
		return "pos"
	case x < 0:
		return "neg"
	}
	return ""
}

func verdictClass(v league.Verdict) string {
	switch v {
	case league.Won:
		return "won"
	case league.Lost:
		return "lost"
	}
	return "close"
}

// resultsAnswer is api/results/{id}: the report as text, for an agent to
// read (league.Report.Text), and its one-line summary.
type resultsAnswer struct {
	ID      string `json:"id"`
	Placed  string `json:"placed"`  // "10th of 10"
	Summary string `json:"summary"` // "beat 5 of 9, lost to 4, too close to call 0"
	Text    string `json:"text"`
}

// report is the report of the table kept under id, or the refusal to
// answer with.
func (s *server) report(id string) (*league.Report, error) {
	if s.records == nil {
		return nil, refuse(http.StatusNotFound, "this table server keeps no ranked tables' results")
	}
	rec, err := s.records.Record(id)
	if errors.Is(err, league.ErrNoRecord) {
		return nil, refuse(http.StatusNotFound, "no ranked table %q", id)
	}
	if err != nil {
		return nil, err
	}
	return league.NewReport(rec)
}

func (s *server) resultsAPI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	rep, err := s.report(id)
	if err != nil {
		w.WriteHeader(failStatus(r, err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(resultsAnswer{ID: id, Placed: rep.Placed(), Summary: rep.Summary(), Text: rep.Text()})
}

func (s *server) resultsPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Cache-Control", "no-store")
	rep, err := s.report(id)
	if err != nil {
		http.Error(w, err.Error(), failStatus(r, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := resultsPage.Execute(w, struct {
		ID string
		*league.Report
	}{id, rep}); err != nil {
		log.Printf("table: %s: %v", r.URL.Path, err)
	}
}

// failStatus is a refusal's status, else 500 (and the error logged).
func failStatus(r *http.Request, err error) int {
	var ae apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	log.Printf("table: %s: %v", r.URL.Path, err)
	return http.StatusInternalServerError
}
