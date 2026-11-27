package league

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/bots"
)

// HousePool is the ranked pool: the league's five house bots, the three
// starters and the two trained bots, by id.
var HousePool = []string{"dove", "clam", "shark", "turtle", "ox"}

// Pool is the house bots with the given ids, as entrants, run natively in
// this process: a starter bot (bots.ByName) is code in this binary, so its
// version is the runner's; a trained bot is loaded from the house-bot
// cache (sdk.LoadHouseByID; fetch it with sdk/cmd/house), its version the
// release and its files' sha256, and each pair plays a Clone of the one
// load. Call close when the tables are done.
func Pool(ids ...string) (pool []Entrant, close func() error, err error) {
	var loaded []*sdk.House
	close = func() error {
		var errs []error
		for _, h := range loaded {
			errs = append(errs, h.Close())
		}
		return errors.Join(errs...)
	}
	m := sdk.Houses()
	for _, id := range ids {
		if newBot, ok := bots.ByName[id]; ok {
			pool = append(pool, Entrant{ID: id, Name: bots.Names[id], Version: RunnerVersion(),
				New: func() (sdk.Bot, error) { return newBot(), nil }})
			continue
		}
		bot, ok := m.Bots[id]
		if !ok {
			close()
			return nil, nil, fmt.Errorf("league: no house bot %q (have %s)", id, strings.Join(HousePool, ", "))
		}
		h, err := sdk.LoadHouseByID(id)
		if err != nil {
			close()
			return nil, nil, err
		}
		loaded = append(loaded, h)
		version := fmt.Sprintf("%s %s@sha256:%s %s@sha256:%s", m.Tag,
			bot.Blueprint, m.Files[bot.Blueprint].SHA256, bot.Buckets, m.Files[bot.Buckets].SHA256)
		pool = append(pool, Entrant{ID: id, Name: bot.Name, Version: version,
			New: func() (sdk.Bot, error) { return h.Clone(), nil }})
	}
	return pool, close, nil
}

// RunnerVersion is this binary's source revision, from the build's VCS
// stamp: the commit, with "+dirty" when the tree had changes; "devel" when
// the build has no stamp (go run, go test).
func RunnerVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return "devel"
	}
	return rev + dirty
}
