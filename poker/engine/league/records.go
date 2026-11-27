package league

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Records is where finished tables' records are kept, each under an id.
type Records interface {
	// Record is the record kept under id; ErrNoRecord when there is none.
	Record(id string) (*Record, error)
}

// ErrNoRecord is a Records' answer for an id it keeps nothing under.
var ErrNoRecord = errors.New("league: no such table")

var recordID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidID reports whether id can name a record: 1 to 64 letters, digits,
// '-' and '_'.
func ValidID(id string) bool { return recordID.MatchString(id) }

// RecordDir keeps records as files in one directory, <id>.json, written
// by Record.JSON (poker/arena/cmd/rankedtable play -out).
type RecordDir string

func (d RecordDir) Record(id string) (*Record, error) {
	if !ValidID(id) {
		return nil, ErrNoRecord
	}
	b, err := os.ReadFile(filepath.Join(string(d), id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoRecord
	}
	if err != nil {
		return nil, err
	}
	return ReadRecord(b)
}
