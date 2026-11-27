package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A submitted bot is one WebAssembly module and, optionally, a directory
// of data files it reads at run time (up to 16 GB). The league keeps each
// submission as a numbered version of a named bot of your account's.
// SubmitBot does the whole of it; go run ./sdk/cmd/bot submit is the
// command for it.

// BotVersion is one version of one of your submitted bots.
type BotVersion struct {
	Name       string   `json:"name"`
	Version    int      `json:"version"`
	Submission string   `json:"submission"`
	CreatedAt  int64    `json:"created_at"` // unix seconds
	Wasm       FileHash `json:"wasm"`
	DataFiles  int      `json:"data_files"`
	DataBytes  int64    `json:"data_bytes"`
}

// String is the version on one line: "mybot v2  2026-11-27 03:10 UTC
// wasm 5.1 MB 54917f724b73  data 1 file 1.07 GB".
func (v BotVersion) String() string {
	sha := v.Wasm.SHA256
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("%s v%d  %s  wasm %s %s  data %s %s", v.Name, v.Version,
		time.Unix(v.CreatedAt, 0).UTC().Format("2006-01-02 15:04 UTC"), Bytes(v.Wasm.Size), sha, files(v.DataFiles), Bytes(v.DataBytes))
}

// FileHash is a file as the league knows it: its size and sha256.
type FileHash struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SubmitOptions is what SubmitBot submits.
type SubmitOptions struct {
	Name    string // the bot's name: lowercase letters, digits and '-'
	Wasm    string // the module's file
	DataDir string // the data directory; "" for none
	// Log, if not nil, is told each step in one plain line.
	Log func(string)
	// Upload is the client the files go through (to the upload URLs the
	// league gives, which carry their own permission: no token is sent);
	// nil is one with a 10 minute timeout a request.
	Upload *http.Client
}

// UploadChunk is the most SubmitBot sends in one request: a multiple of
// 256 KiB, as Cloud Storage's resumable uploads require.
const UploadChunk = 8 << 20

// retryWait is how long to wait before the nth retry of an upload.
var retryWait = func(n int) time.Duration { return time.Duration(1<<n) * time.Second }

// submission is the league's answer about a submission.
type submission struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Uploads []struct {
		SHA256  string `json:"sha256"`
		Size    int64  `json:"size"`
		Done    bool   `json:"done"`
		Session string `json:"session"` // where to upload it, if not done
	} `json:"uploads"`
}

type dataFile struct {
	Path string `json:"path"`
	FileHash
	local string
}

// SubmitBot submits a bot: it hashes the files, declares them, uploads
// each one the league does not already have for your account (so a new
// version with the same data sends only its module), and asks the league
// to check and keep it. Run it again after a failure: a submission of the
// same files picks up where the last one stopped.
func (t *Table) SubmitBot(ctx context.Context, opt SubmitOptions) (BotVersion, error) {
	say := opt.Log
	if say == nil {
		say = func(string) {}
	}
	wasm, err := os.Stat(opt.Wasm)
	if err != nil {
		return BotVersion{}, err
	}
	if !wasm.Mode().IsRegular() {
		return BotVersion{}, fmt.Errorf("sdk: %s is not a file", opt.Wasm)
	}
	var data []dataFile
	if opt.DataDir != "" {
		if data, err = listData(opt.DataDir); err != nil {
			return BotVersion{}, err
		}
	}
	var dataBytes int64
	for _, f := range data {
		dataBytes += f.Size
	}
	say(fmt.Sprintf("bot %s: wasm %s %s; data %s %s", opt.Name, opt.Wasm, Bytes(wasm.Size()), files(len(data)), Bytes(dataBytes)))

	start := time.Now()
	wasmHash, err := hashFile(opt.Wasm)
	if err != nil {
		return BotVersion{}, err
	}
	local := map[string]string{wasmHash.SHA256: opt.Wasm}
	for i := range data {
		if data[i].FileHash, err = hashFile(data[i].local); err != nil {
			return BotVersion{}, err
		}
		local[data[i].SHA256] = data[i].local
	}
	say(fmt.Sprintf("hashed %s in %.1f s", Bytes(wasm.Size()+dataBytes), time.Since(start).Seconds()))

	api := *t
	if api.HTTP == nil {
		// Finalizing reads every new file back: minutes for many GB.
		api.HTTP = &http.Client{Timeout: 30 * time.Minute}
	}
	if data == nil {
		data = []dataFile{}
	}
	var sub submission
	if err := api.call(ctx, "POST", "api/bots/submissions", map[string]any{"name": opt.Name, "wasm": wasmHash, "data": data}, &sub); err != nil {
		return BotVersion{}, err
	}
	var todo, sending int64
	for _, u := range sub.Uploads {
		if !u.Done {
			todo++
			sending += u.Size
		}
	}
	say(fmt.Sprintf("submission %s: %s to upload, %s", sub.ID, files(int(todo)), Bytes(sending)))

	up := opt.Upload
	if up == nil {
		up = &http.Client{Timeout: 10 * time.Minute}
	}
	for _, u := range sub.Uploads {
		if u.Done {
			continue
		}
		if u.Session == "" {
			return BotVersion{}, fmt.Errorf("sdk: the league gave no upload session for %s", local[u.SHA256])
		}
		begun := time.Now()
		say(fmt.Sprintf("upload %s %s", local[u.SHA256], Bytes(u.Size)))
		if err := uploadFile(ctx, up, u.Session, local[u.SHA256], u.Size, say); err != nil {
			return BotVersion{}, fmt.Errorf("sdk: uploading %s: %w (run the same submit again to resume)", local[u.SHA256], err)
		}
		secs := time.Since(begun).Seconds()
		say(fmt.Sprintf("uploaded %s in %.1f s (%s/s)", local[u.SHA256], secs, Bytes(int64(float64(u.Size)/max(secs, 1e-3)))))
	}

	say("finalize: the league checks every file and the module")
	var done struct {
		Bot BotVersion `json:"bot"`
	}
	if err := api.call(ctx, "POST", "api/bots/submissions/"+sub.ID+"/finalize", nil, &done); err != nil {
		return BotVersion{}, err
	}
	return done.Bot, nil
}

// Bots lists your account's submitted bots, every version, by name.
func (t *Table) Bots(ctx context.Context) ([]BotVersion, error) {
	var out struct {
		Bots []BotVersion `json:"bots"`
	}
	err := t.call(ctx, "GET", "api/bots", nil, &out)
	return out.Bots, err
}

// listData is every file under dir, by its path from dir with '/'
// between parts, sorted. Only regular files: a link, or a name the league
// refuses (hidden: starting with '.'), is an error naming it.
func listData(dir string) ([]dataFile, error) {
	var out []dataFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(d.Name(), ".") {
			return fmt.Errorf("sdk: %s: the league does not take hidden files or folders in a bot's data; remove or rename it", rel)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("sdk: %s is not a regular file (a link?); put the file itself in the data folder", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, dataFile{Path: rel, FileHash: FileHash{Size: info.Size()}, local: p})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func hashFile(path string) (FileHash, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileHash{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileHash{}, err
	}
	return FileHash{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// uploadFile sends the file at path, size bytes, to an upload session by
// Cloud Storage's resumable upload protocol: it asks the session how much
// it has (a run before this one may have sent some), then PUTs the rest
// UploadChunk bytes at a time. After a failed PUT it asks again and goes
// on from there, a few times before giving up.
func uploadFile(ctx context.Context, client *http.Client, session, path string, size int64, say func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var have int64
	failures, next, query, first := 0, size/10, true, true
	for {
		n := int64(0) // a status query: how much the session has
		if !query && have < size {
			n = min(int64(UploadChunk), size-have)
		}
		res, body, err := putChunk(ctx, client, session, f, have, n, size)
		query = false
		if err == nil {
			switch {
			case res.StatusCode == http.StatusOK || res.StatusCode == http.StatusCreated:
				return nil
			case res.StatusCode == http.StatusPermanentRedirect:
				if have, err = rangeEnd(res.Header.Get("Range")); err != nil {
					return err
				}
				if have == size {
					return errors.New("the upload session has every byte but did not finish the file")
				}
				failures = 0
				if first && have > 0 {
					say(fmt.Sprintf("  resuming at %s", Bytes(have)))
				}
				first = false
				if size >= 100*UploadChunk && have >= next {
					say(fmt.Sprintf("  %d%% %s", 100*have/size, Bytes(have)))
					next = have + size/10
				}
				continue
			case res.StatusCode < 500 && res.StatusCode != http.StatusTooManyRequests:
				return uploadError(res, body)
			}
		}
		if failures++; failures > 5 {
			if err != nil {
				return err
			}
			return uploadError(res, body)
		}
		why := ""
		if err != nil {
			why = err.Error()
		} else {
			why = res.Status
		}
		say(fmt.Sprintf("  retry %d after: %s", failures, why))
		select {
		case <-time.After(retryWait(failures)):
		case <-ctx.Done():
			return ctx.Err()
		}
		query = true // the session may have kept part of the chunk: ask before sending more
	}
}

// putChunk PUTs n bytes of f from offset `from`; n == 0 is a status query
// ("bytes */size").
func putChunk(ctx context.Context, client *http.Client, session string, f *os.File, from, n, size int64) (*http.Response, []byte, error) {
	var body io.Reader = http.NoBody
	rng := fmt.Sprintf("bytes */%d", size)
	if n > 0 {
		body = io.NewSectionReader(f, from, n)
		rng = fmt.Sprintf("bytes %d-%d/%d", from, from+n-1, size)
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", session, body)
	if err != nil {
		return nil, nil, err
	}
	req.ContentLength = n
	req.Header.Set("Content-Range", rng)
	return send(client, req)
}

// rangeEnd is how many bytes a session has, from its Range header
// ("bytes=0-N"; none: 0).
func rangeEnd(h string) (int64, error) {
	if h == "" {
		return 0, nil
	}
	_, last, ok := strings.Cut(strings.TrimPrefix(h, "bytes="), "-")
	n, err := strconv.ParseInt(last, 10, 64)
	if !ok || err != nil || !strings.HasPrefix(h, "bytes=0-") {
		return 0, fmt.Errorf("an upload Range header %q", h)
	}
	return n + 1, nil
}

func uploadError(res *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return fmt.Errorf("%s: %s", res.Status, msg)
}

// files is "1 file", "2 files".
func files(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// Bytes is n as a short size: "512 B", "5.2 MB", "1.07 GB" (SI units).
func Bytes(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1000*1000:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	}
}
