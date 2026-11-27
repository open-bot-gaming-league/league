package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The release's terms, exactly, with their newline: what the manifest
// pins NOTICE.txt to.
const noticeText = "Free to download and use with the Open Bot Gaming League SDK, for your own play and research. You may not redistribute these files. No other rights are granted. We expect to open this up after a legal review.\n"

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestHouseManifest(t *testing.T) {
	m := Houses()
	if m.Tag != "house-bots-1" || !strings.HasSuffix(m.BaseURL, "/releases/download/"+m.Tag+"/") {
		t.Fatalf("tag %q, base %q", m.Tag, m.BaseURL)
	}
	if n := m.Files[HouseNotice]; n.SHA256 != sum([]byte(noticeText)) || n.Size != int64(len(noticeText)) {
		t.Fatalf("NOTICE.txt pinned to %+v, not the notice text", n)
	}
	for id, bot := range m.Bots {
		for _, name := range []string{bot.Buckets, bot.Blueprint} {
			if f, ok := m.Files[name]; !ok || f.Size <= 0 || len(f.SHA256) != 64 {
				t.Errorf("%s: file %s: %+v", id, name, f)
			}
		}
	}
	if m.Bots["turtle"].Name != "Turtle" || m.Bots["ox"].Name != "Ox" {
		t.Errorf("bots: %+v", m.Bots)
	}
}

// fakeRelease serves a tiny release, counting requests; broken names get
// the wrong bytes, missing names 404.
type fakeRelease struct {
	files   map[string][]byte
	broken  map[string]bool
	missing map[string]bool
	mu      sync.Mutex
	hits    []string
	ranges  []string
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	f.hits = append(f.hits, name)
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	f.mu.Unlock()
	body, ok := f.files[name]
	if !ok || f.missing[name] {
		http.NotFound(w, r)
		return
	}
	if f.broken[name] {
		body = bytes.ToUpper(body)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body)) // ranges too
}

func tinyRelease() (*fakeRelease, HouseManifest) {
	f := &fakeRelease{files: map[string][]byte{
		HouseNotice: []byte(noticeText),
		"b1.bin":    []byte("buckets one"),
		"b2.bin":    []byte("buckets two"),
		"t.bp":      []byte("turtle's blueprint"),
		"o.bp":      []byte("ox's blueprint"),
	}, broken: map[string]bool{}, missing: map[string]bool{}}
	m := HouseManifest{Tag: "test-1", Files: map[string]HouseFile{}}
	for name, b := range f.files {
		m.Files[name] = HouseFile{Size: int64(len(b)), SHA256: sum(b)}
	}
	m.Bots = map[string]struct {
		Name      string `json:"name"`
		Buckets   string `json:"buckets"`
		Blueprint string `json:"blueprint"`
	}{"turtle": {"Turtle", "b1.bin", "t.bp"}, "ox": {"Ox", "b2.bin", "o.bp"}}
	return f, m
}

func TestDownloadHouse(t *testing.T) {
	f, m := tinyRelease()
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	var out bytes.Buffer
	d := HouseDownload{Manifest: m, BaseURL: srv.URL, Dir: dir, Out: &out}

	// One bot: its two files and NOTICE.txt, nothing else, said first.
	if err := d.Get(context.Background(), "turtle"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.hits, " "); got != "NOTICE.txt b1.bin t.bp" {
		t.Fatalf("fetched %q", got)
	}
	if !strings.Contains(out.String(), "downloading 3 file(s)") {
		t.Errorf("did not say what it would fetch: %q", out.String())
	}
	for name, want := range map[string]string{HouseNotice: noticeText, "b1.bin": "buckets one", "t.bp": "turtle's blueprint"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	// Already there: no fetch at all.
	f.hits = nil
	if err := d.Get(context.Background(), "turtle"); err != nil || len(f.hits) != 0 {
		t.Fatalf("cached: fetched %v, %v", f.hits, err)
	}
	// "all" fetches only what is missing (NOTICE is there).
	if err := d.Get(context.Background(), "all"); err != nil || strings.Join(f.hits, " ") != "b2.bin o.bp" {
		t.Fatalf("all: fetched %v, %v", f.hits, err)
	}
	// A cached file gone bad is fetched again.
	os.WriteFile(filepath.Join(dir, "t.bp"), []byte("turtle's blueprinX"), 0o644)
	f.hits = nil
	if err := d.Get(context.Background(), "turtle"); err != nil || strings.Join(f.hits, " ") != "t.bp" {
		t.Fatalf("bad cache: fetched %v, %v", f.hits, err)
	}
}

func TestDownloadHouseRefusesAWrongFile(t *testing.T) {
	f, m := tinyRelease()
	f.broken["t.bp"] = true
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	err := HouseDownload{Manifest: m, BaseURL: srv.URL, Dir: dir, Out: &bytes.Buffer{}}.Get(context.Background(), "turtle")
	if err == nil || !strings.Contains(err.Error(), "t.bp came down wrong") {
		t.Fatalf("%v", err)
	}
	for _, name := range []string{"t.bp", "t.bp.part"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s left behind", name)
		}
	}
}

func TestDownloadHouseNeedsTheNotice(t *testing.T) {
	f, m := tinyRelease()
	f.missing[HouseNotice] = true
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	err := HouseDownload{Manifest: m, BaseURL: srv.URL, Dir: dir, Out: &bytes.Buffer{}}.Get(context.Background(), "ox")
	if err == nil || !strings.Contains(err.Error(), "NOTICE.txt is not there") || !strings.Contains(err.Error(), "isn't published yet") {
		t.Fatalf("%v", err)
	}
	if strings.Join(f.hits, " ") != "NOTICE.txt" {
		t.Errorf("fetched %v: the bot files before the notice", f.hits)
	}
	// A wrong notice is refused like any wrong file.
	f.missing[HouseNotice], f.broken[HouseNotice] = false, true
	err = HouseDownload{Manifest: m, BaseURL: srv.URL, Dir: dir, Out: &bytes.Buffer{}}.Get(context.Background(), "ox")
	if err == nil || !strings.Contains(err.Error(), "NOTICE.txt came down wrong") {
		t.Fatalf("a wrong notice: %v", err)
	}
}

func TestDownloadHouseResumes(t *testing.T) {
	f, m := tinyRelease()
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "o.bp.part"), []byte("ox's "), 0o644) // a download cut off
	if err := (HouseDownload{Manifest: m, BaseURL: srv.URL, Dir: dir, Out: &bytes.Buffer{}}).Get(context.Background(), "ox"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "o.bp")); string(got) != "ox's blueprint" {
		t.Fatalf("resumed: %q", got)
	}
	for i, name := range f.hits {
		if name == "o.bp" && f.ranges[i] != "bytes=5-" {
			t.Errorf("o.bp asked with Range %q", f.ranges[i])
		}
	}
}

func TestLoadHouseByIDSaysHowToDownload(t *testing.T) {
	t.Setenv("OBGL_HOUSE_CACHE", t.TempDir())
	_, err := LoadHouseByID("turtle")
	if err == nil || !strings.Contains(err.Error(), "go run ./sdk/cmd/house download turtle") {
		t.Fatalf("%v", err)
	}
	if _, err := LoadHouseByID("walrus"); err == nil || !strings.Contains(err.Error(), "turtle") {
		t.Fatalf("%v", err)
	}
}
