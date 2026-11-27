package arena

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/open-bot-gaming-league/league/poker/engine/league"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// Ranked tables: wasm bots as league entrants.

// Entrant is p as a seat at a ranked table: each pair the seat plays
// starts its own instance with cfg. The bot is a league.Meter and is
// closed when its pair is done.
func Entrant(id, name, version string, p *Program, cfg Config) league.Entrant {
	return league.Entrant{ID: id, Name: name, Version: version, New: func() (sdk.Bot, error) {
		b, err := p.Start(cfg)
		if err != nil {
			return nil, err
		}
		return b, nil
	}}
}

// Usage is the bot's use for its league record: fuel of the load and the
// decisions, bytes read from its data, and its linear memory (the host's
// own overhead is not counted).
func (b *Bot) Usage() league.Usage {
	r := b.Report()
	u := league.Usage{
		Fuel: int64(r.StartFuel + r.Fuel), BytesRead: r.ReadBytes, MemoryPeak: int64(r.MemoryBytes),
		Forfeited: int64(r.Forfeited()),
	}
	if f := r.Fault; f != nil {
		u.Fault = &league.Fault{Kind: string(f.Kind), Decision: f.Decision, Hand: f.Hand, Detail: f.Detail}
	}
	return u
}

var _ league.Meter = (*Bot)(nil)

// Build compiles the Go bot in package directory dir to a wasm bot
// module (a WASI reactor) with the local Go toolchain; flags are added to
// go build's (say, -tags=...).
func Build(dir string, flags ...string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "arena-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "bot.wasm")
	cmd := exec.Command("go", append(append([]string{"build", "-buildmode=c-shared", "-o", out}, flags...), ".")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("arena: building %s: %v\n%s", dir, err, b)
	}
	return os.ReadFile(out)
}

// WasmPool is the house bots with the given ids (league.HousePool's) as
// wasm entrants: the guests in guestsDir (this module's guests/) built and
// compiled once, each pair starting fresh instances. A trained bot's data
// is its two files from the house-bot cache, mounted as /data, and its
// version is the module's sha256 and the release's files.
func WasmPool(guestsDir string, ids ...string) ([]league.Entrant, error) {
	progs := map[string]*Program{}
	program := func(guest string) (*Program, error) {
		if p := progs[guest]; p != nil {
			return p, nil
		}
		wasm, err := Build(filepath.Join(guestsDir, guest))
		if err != nil {
			return nil, err
		}
		p, err := Compile(wasm)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", guest, err)
		}
		progs[guest] = p
		return p, nil
	}
	m := sdk.Houses()
	var pool []league.Entrant
	for _, id := range ids {
		if _, ok := bots.ByName[id]; ok {
			p, err := program(id)
			if err != nil {
				return nil, err
			}
			pool = append(pool, Entrant(id, bots.Names[id], "wasm sha256:"+p.SHA256(), p, Config{}))
			continue
		}
		hb, ok := m.Bots[id]
		if !ok {
			return nil, fmt.Errorf("arena: no house bot %q", id)
		}
		data, err := HouseData(id)
		if err != nil {
			return nil, err
		}
		p, err := program("house")
		if err != nil {
			return nil, err
		}
		pool = append(pool, Entrant(id, hb.Name, "wasm sha256:"+p.SHA256()+" "+league.HouseVersion(id), p, Config{Data: data}))
	}
	return pool, nil
}

// HouseData is a trained house bot's files in the house-bot cache, as the
// house guest's data: /data/buckets.bin and /data/blueprint.bp.
func HouseData(id string) (Files, error) {
	hb, ok := sdk.Houses().Bots[id]
	if !ok {
		return nil, fmt.Errorf("arena: no house bot %q", id)
	}
	dir, err := sdk.HouseCacheDir()
	if err != nil {
		return nil, err
	}
	f := Files{"buckets.bin": filepath.Join(dir, hb.Buckets), "blueprint.bp": filepath.Join(dir, hb.Blueprint)}
	for _, path := range f {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("arena: %s is not downloaded (%v): go run ./sdk/cmd/house download %s, in poker/engine", hb.Name, err, id)
		}
	}
	return f, nil
}
