package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLeague is the league's submission API and an upload session store
// that speak the protocols SubmitBot uses, with a fault to inject: the
// first PUT of a chunk past cutAt keeps only half the chunk and drops the
// connection.
type fakeLeague struct {
	t       *testing.T
	mu      sync.Mutex
	have    map[string][]byte // upload sessions by sha256
	known   map[string]bool   // files the account already has
	cutAt   int64
	cut     bool
	down    bool  // after the cut, every PUT is a 503
	got     int64 // body bytes received by PUTs
	sizes   map[string]int64
	started int // upload sessions
}

func (f *fakeLeague) handler(srv **httptest.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /table/api/bots/submissions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer obgl_pt_test" {
			http.Error(w, `{"error":"no token"}`, 401)
			return
		}
		var m struct {
			Name string     `json:"name"`
			Wasm FileHash   `json:"wasm"`
			Data []dataFile `json:"data"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		var ups []map[string]any
		f.mu.Lock()
		for _, h := range append([]FileHash{m.Wasm}, func() (hs []FileHash) {
			for _, d := range m.Data {
				hs = append(hs, d.FileHash)
			}
			return
		}()...) {
			f.sizes[h.SHA256] = h.Size
			if f.known[h.SHA256] {
				continue
			}
			if _, ok := f.have[h.SHA256]; !ok {
				f.have[h.SHA256] = []byte{}
				f.started++
			}
			ups = append(ups, map[string]any{"sha256": h.SHA256, "size": h.Size, "done": int64(len(f.have[h.SHA256])) == h.Size,
				"session": (*srv).URL + "/s/" + h.SHA256})
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": "s1", "state": "open", "uploads": ups})
	})
	mux.HandleFunc("POST /table/api/bots/submissions/s1/finalize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for sha, b := range f.have {
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) != sha {
				http.Error(w, `{"error":"bad bytes for `+sha+`"}`, 422)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"bot": BotVersion{Name: "mybot", Version: 1, CreatedAt: 1800000000}})
	})
	rng := regexp.MustCompile(`^bytes (?:(\d+)-(\d+)|\*)/(\d+)$`)
	mux.HandleFunc("PUT /s/{sha}", func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		m := rng.FindStringSubmatch(r.Header.Get("Content-Range"))
		f.mu.Lock()
		size := f.sizes[sha]
		f.mu.Unlock()
		if m == nil || m[3] != strconv.FormatInt(size, 10) {
			http.Error(w, "bad range "+r.Header.Get("Content-Range"), 400)
			return
		}
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "a token sent to an upload session", 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		if f.down && f.cut {
			f.mu.Unlock()
			http.Error(w, "down", 503)
			return
		}
		f.got += int64(len(body))
		have := f.have[sha]
		if m[1] != "" {
			first, _ := strconv.ParseInt(m[1], 10, 64)
			if first != int64(len(have)) {
				f.mu.Unlock()
				http.Error(w, "not where the session is", 400)
				return
			}
			if !f.cut && f.cutAt > 0 && first >= f.cutAt {
				f.cut = true
				f.have[sha] = append(have, body[:len(body)/2]...)
				f.mu.Unlock()
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			have = append(have, body...)
			f.have[sha] = have
		}
		f.mu.Unlock()
		if int64(len(have)) == size {
			w.WriteHeader(200)
			return
		}
		if len(have) > 0 {
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(have)-1))
		}
		w.WriteHeader(308)
	})
	return mux
}

func newFake(t *testing.T) (*fakeLeague, *Table) {
	wait := retryWait
	retryWait = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryWait = wait })
	f := &fakeLeague{t: t, have: map[string][]byte{}, known: map[string]bool{}, sizes: map[string]int64{}}
	var srv *httptest.Server
	srv = httptest.NewServer(f.handler(&srv))
	t.Cleanup(srv.Close)
	return f, &Table{URL: srv.URL + "/table/", Token: "obgl_pt_test"}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitBotUploadsInChunksAndResumesACutChunk(t *testing.T) {
	f, table := newFake(t)
	dir := t.TempDir()
	big := make([]byte, 3*UploadChunk+12345)
	for i := range big {
		big[i] = byte(i * 7)
	}
	writeFile(t, filepath.Join(dir, "bot.wasm"), []byte("\x00asm\x01\x00\x00\x00"))
	writeFile(t, filepath.Join(dir, "data", "tables", "big.bin"), big)
	writeFile(t, filepath.Join(dir, "data", "empty.bin"), nil)
	f.cutAt = UploadChunk // the second chunk of big.bin is cut in half
	var log []string
	v, err := table.SubmitBot(context.Background(), SubmitOptions{Name: "mybot", Wasm: filepath.Join(dir, "bot.wasm"),
		DataDir: filepath.Join(dir, "data"), Log: func(s string) { log = append(log, s) }})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(log, "\n"))
	}
	if v.Name != "mybot" || v.Version != 1 || !f.cut {
		t.Fatalf("%+v cut=%v", v, f.cut)
	}
	want := int64(len(big) + 8)
	// The cut chunk's first half arrived (counted in got) and was kept; only its second half is sent again.
	if f.got != want+UploadChunk/2 {
		t.Errorf("sent %d body bytes for %d bytes of files: want exactly half a chunk resent (%d)", f.got, want, want+UploadChunk/2)
	}
	if f.started != 3 {
		t.Errorf("%d upload sessions, want 3 (the module, big.bin, the empty file)", f.started)
	}
	if strings.Contains(strings.Join(log, "\n"), "resuming") {
		t.Errorf("a first run resumed:\n%s", strings.Join(log, "\n"))
	}
	if !strings.HasPrefix(log[0], "bot mybot: wasm ") || !strings.Contains(log[0], "data 2 files 25.2 MB") {
		t.Errorf("first line %q: want the sizes first", log[0])
	}
	if !strings.Contains(strings.Join(log, "\n"), "retry 1 after:") {
		t.Errorf("no retry line in\n%s", strings.Join(log, "\n"))
	}
}

func TestSubmitBotRunAgainResumesWhereTheLastRunStopped(t *testing.T) {
	f, table := newFake(t)
	dir := t.TempDir()
	big := make([]byte, 2*UploadChunk+1)
	writeFile(t, filepath.Join(dir, "bot.wasm"), []byte("\x00asm\x01\x00\x00\x00"))
	writeFile(t, filepath.Join(dir, "data", "big.bin"), big)
	f.cutAt, f.down = UploadChunk, true
	opt := SubmitOptions{Name: "mybot", Wasm: filepath.Join(dir, "bot.wasm"), DataDir: filepath.Join(dir, "data")}
	_, err := table.SubmitBot(context.Background(), opt)
	if err == nil || !strings.Contains(err.Error(), "run the same submit again to resume") {
		t.Fatalf("%v", err)
	}
	f.down = false
	var log []string
	opt.Log = func(s string) { log = append(log, s) }
	if _, err := table.SubmitBot(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if want := int64(len(big)+8) + UploadChunk/2; f.got != want {
		t.Errorf("sent %d body bytes, want %d: the file and half a chunk again", f.got, want)
	}
	if !strings.Contains(strings.Join(log, "\n"), "  resuming at 12.6 MB") {
		t.Errorf("no resume line in\n%s", strings.Join(log, "\n"))
	}
}

func TestSubmitBotSendsOnlyWhatTheLeagueLacks(t *testing.T) {
	f, table := newFake(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bot.wasm"), []byte("\x00asm\x01\x00\x00\x00"))
	writeFile(t, filepath.Join(dir, "data", "d.bin"), []byte("the data"))
	sum := sha256.Sum256([]byte("the data"))
	f.known[hex.EncodeToString(sum[:])] = true
	if _, err := table.SubmitBot(context.Background(), SubmitOptions{Name: "mybot", Wasm: filepath.Join(dir, "bot.wasm"), DataDir: filepath.Join(dir, "data")}); err != nil {
		t.Fatal(err)
	}
	if f.got != 8 || f.started != 1 {
		t.Errorf("sent %d bytes in %d sessions: want only the module's 8 in 1", f.got, f.started)
	}
}

func TestSubmitBotRefusesDataTheLeagueWouldNot(t *testing.T) {
	_, table := newFake(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bot.wasm"), []byte("\x00asm\x01\x00\x00\x00"))
	writeFile(t, filepath.Join(dir, "data", "ok.bin"), []byte("x"))
	writeFile(t, filepath.Join(dir, "data", "sub", ".DS_Store"), []byte("x"))
	_, err := table.SubmitBot(context.Background(), SubmitOptions{Name: "mybot", Wasm: filepath.Join(dir, "bot.wasm"), DataDir: filepath.Join(dir, "data")})
	if err == nil || !strings.Contains(err.Error(), "sub/.DS_Store: the league does not take hidden files") {
		t.Fatalf("%v", err)
	}
	os.Remove(filepath.Join(dir, "data", "sub", ".DS_Store"))
	if err := os.Symlink("ok.bin", filepath.Join(dir, "data", "link.bin")); err != nil {
		t.Fatal(err)
	}
	_, err = table.SubmitBot(context.Background(), SubmitOptions{Name: "mybot", Wasm: filepath.Join(dir, "bot.wasm"), DataDir: filepath.Join(dir, "data")})
	if err == nil || !strings.Contains(err.Error(), "link.bin is not a regular file") {
		t.Fatalf("%v", err)
	}
	table.Token = "wrong"
	os.Remove(filepath.Join(dir, "data", "link.bin"))
	_, err = table.SubmitBot(context.Background(), SubmitOptions{Name: "mybot", Wasm: filepath.Join(dir, "bot.wasm")})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("%v", err)
	}
}

func TestRangeEnd(t *testing.T) {
	for h, want := range map[string]int64{"": 0, "bytes=0-0": 1, "bytes=0-8388607": 8 << 20} {
		if got, err := rangeEnd(h); err != nil || got != want {
			t.Errorf("%q: %d, %v", h, got, err)
		}
	}
	for _, h := range []string{"bytes=5-9", "bytes 0-9", "bytes=0-x"} {
		if _, err := rangeEnd(h); err == nil {
			t.Errorf("%q: no error", h)
		}
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 5_242_880: "5.2 MB", 1 << 30: "1.07 GB", 16 << 30: "17.18 GB"} {
		if got := Bytes(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}
