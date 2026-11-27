package sdk

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The house bots Turtle and Ox are files too big for the repository
// (about 850 MB for both). They are published as a release of the league's
// repository, pinned here by name, size and sha256: house-bots.json. A
// changed bot is a new release tag, never an edited file. NOTICE.txt, the
// terms the files come under, is one of the release's files and is always
// downloaded with them.

//go:embed house-bots.json
var houseManifestJSON []byte

// HouseManifest is house-bots.json: the release, its files, and which
// files make each house bot.
type HouseManifest struct {
	Tag     string               `json:"tag"`
	BaseURL string               `json:"base_url"`
	Files   map[string]HouseFile `json:"files"`
	Bots    map[string]struct {
		Name      string `json:"name"`
		Buckets   string `json:"buckets"`
		Blueprint string `json:"blueprint"`
	} `json:"bots"`
}

// HouseFile is one file of the release.
type HouseFile struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// HouseNotice is the release's terms file, fetched and checked with every
// download.
const HouseNotice = "NOTICE.txt"

// Houses is the manifest built into the SDK.
func Houses() HouseManifest {
	var m HouseManifest
	if err := json.Unmarshal(houseManifestJSON, &m); err != nil {
		panic(err) // embedded at build time; TestHouseManifest reads it
	}
	return m
}

// HouseCacheDir is where downloaded house bots live: $OBGL_HOUSE_CACHE if
// set, else obgl/<release tag> in the user's cache directory (on Linux
// ~/.cache/obgl/house-bots-1, on macOS ~/Library/Caches/obgl/house-bots-1,
// on Windows %LocalAppData%\obgl\house-bots-1).
func HouseCacheDir() (string, error) {
	if dir := os.Getenv("OBGL_HOUSE_CACHE"); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "obgl", Houses().Tag), nil
}

// HouseDownload fetches house bots into Dir. The zero value of each field
// is the usual one: the built-in manifest and its release, HouseCacheDir,
// a client with no overall timeout, and progress lines on stderr.
type HouseDownload struct {
	Manifest HouseManifest // zero: Houses()
	BaseURL  string
	Dir      string
	HTTP     *http.Client
	Out      io.Writer
}

// DownloadHouse fetches house bots by id ("turtle", "ox", or "all") into
// the cache, as HouseDownload with its defaults does.
func DownloadHouse(ctx context.Context, ids ...string) error {
	return HouseDownload{}.Get(ctx, ids...)
}

// Get fetches the files the bots need, and NOTICE.txt, into Dir: it says
// how much it will fetch first, skips a file already there with the right
// size and sha256, picks up a partial download where it stopped, checks
// every file's sha256, and only then gives it its name. A file that comes
// out wrong is deleted and reported.
func (d HouseDownload) Get(ctx context.Context, ids ...string) error {
	m := d.Manifest
	if m.Tag == "" {
		m = Houses()
	}
	if d.BaseURL == "" {
		d.BaseURL = m.BaseURL
	}
	if d.Dir == "" {
		dir, err := HouseCacheDir()
		if err != nil {
			return err
		}
		d.Dir = dir
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{}
	}
	if d.Out == nil {
		d.Out = os.Stderr
	}
	names, err := houseFiles(m, ids)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	var todo []string
	var bytes int64
	for _, name := range names {
		if ok, err := haveFile(filepath.Join(d.Dir, name), m.Files[name]); err != nil {
			return err
		} else if !ok {
			todo = append(todo, name)
			bytes += m.Files[name].Size
		}
	}
	if len(todo) == 0 {
		fmt.Fprintf(d.Out, "house bots: %s already in %s\n", strings.Join(ids, ", "), d.Dir)
		return nil
	}
	fmt.Fprintf(d.Out, "house bots: downloading %d file(s), %.0f MB, to %s\n", len(todo), float64(bytes)/1e6, d.Dir)
	for _, name := range todo {
		if err := d.fetch(ctx, m, name); err != nil {
			return err
		}
	}
	return nil
}

// houseFiles is the files the bots need, NOTICE.txt first, each once.
func houseFiles(m HouseManifest, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("sdk: which house bot? (turtle, ox, or all)")
	}
	want := map[string]bool{}
	var bots []string
	for _, id := range ids {
		if id == "all" {
			for id := range m.Bots {
				bots = append(bots, id)
			}
			continue
		}
		if _, ok := m.Bots[id]; !ok {
			return nil, fmt.Errorf("sdk: no house bot %q (%s, or all)", id, strings.Join(houseIDs(m), ", "))
		}
		bots = append(bots, id)
	}
	for _, id := range bots {
		want[m.Bots[id].Buckets], want[m.Bots[id].Blueprint] = true, true
	}
	names := []string{HouseNotice}
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	return names, nil
}

func houseIDs(m HouseManifest) []string {
	var ids []string
	for id := range m.Bots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// haveFile reports whether path holds the file the manifest names: its
// size, then its sha256. A file of the right size but the wrong hash is
// removed, to be fetched again.
func haveFile(path string, f HouseFile) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if info.Size() == f.Size {
		sum, err := fileSHA256(path)
		if err != nil {
			return false, err
		}
		if sum == f.SHA256 {
			return true, nil
		}
	}
	return false, os.Remove(path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetch downloads one file to name.part (resuming one that is there),
// checks it, and renames it.
func (d HouseDownload) fetch(ctx context.Context, m HouseManifest, name string) error {
	want := m.Files[name]
	final := filepath.Join(d.Dir, name)
	part := final + ".part"
	var have int64
	if info, err := os.Stat(part); err == nil && info.Size() < want.Size {
		have = info.Size()
	} else {
		os.Remove(part)
	}
	url := strings.TrimRight(d.BaseURL, "/") + "/" + name
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	res, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("sdk: downloading %s: %w", name, err)
	}
	defer res.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch res.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC
		have = 0
	case http.StatusNotFound:
		return fmt.Errorf("sdk: %s is not there (404 from %s): the house-bot release %s isn't published yet, or not under that name; try again later", name, url, m.Tag)
	default:
		return fmt.Errorf("sdk: downloading %s: %s from %s", name, res.Status, url)
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	start := time.Now()
	n, err := io.Copy(f, res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sdk: downloading %s (got %d of %d bytes; run it again to pick up from there): %w", name, have+n, want.Size, err)
	}
	sum, err := fileSHA256(part)
	if err != nil {
		return err
	}
	if sum != want.SHA256 {
		os.Remove(part)
		return fmt.Errorf("sdk: %s came down wrong (sha256 %s, want %s); deleted it, run the download again", name, sum, want.SHA256)
	}
	if err := os.Rename(part, final); err != nil {
		return err
	}
	fmt.Fprintf(d.Out, "house bots: %s ok (%.0f MB, %s)\n", name, float64(want.Size)/1e6, time.Since(start).Round(time.Second))
	return nil
}

// LoadHouseByID loads a downloaded house bot ("turtle", "ox") from the
// cache. It checks that the files and NOTICE.txt are there at their sizes
// (DownloadHouse checked their sha256), and says how to download them when
// they are not.
func LoadHouseByID(id string) (*House, error) {
	m := Houses()
	bot, ok := m.Bots[id]
	if !ok {
		return nil, fmt.Errorf("sdk: no house bot %q (%s)", id, strings.Join(houseIDs(m), ", "))
	}
	dir, err := HouseCacheDir()
	if err != nil {
		return nil, err
	}
	for _, name := range []string{HouseNotice, bot.Buckets, bot.Blueprint} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() != m.Files[name].Size {
			return nil, fmt.Errorf("sdk: %s is not downloaded (no %s in %s): go run ./sdk/cmd/house download %s", bot.Name, name, dir, id)
		}
	}
	return LoadHouse(filepath.Join(dir, bot.Buckets), filepath.Join(dir, bot.Blueprint))
}
