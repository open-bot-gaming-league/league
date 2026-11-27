package arena

import (
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
)

// Files is a flat data mount: each name in it is a file of the bot's
// /data, read from the host path it maps to. It is how one set of files
// on the host serves several bots, each seeing only its own (the house
// bots' bucket files, say). Names may not contain a slash.
type Files map[string]string

var (
	_ fs.ReadDirFS = Files(nil)
	_ fs.StatFS    = Files(nil)
)

func (f Files) Open(name string) (fs.File, error) {
	if name == "." {
		return &filesDir{f: f}, nil
	}
	host, err := f.host("open", name)
	if err != nil {
		return nil, err
	}
	return os.Open(host)
}

func (f Files) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return dirInfo{}, nil
	}
	host, err := f.host("stat", name)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(host)
	if err != nil {
		return nil, err
	}
	return fileInfo{name: name, FileInfo: info}, nil
}

func (f Files) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	var out []fs.DirEntry
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for n := range f {
			if !yield(n) {
				return
			}
		}
	}) {
		info, err := f.Stat(n)
		if err != nil {
			return nil, err
		}
		out = append(out, fs.FileInfoToDirEntry(info))
	}
	return out, nil
}

func (f Files) host(op, name string) (string, error) {
	host, ok := f[name]
	if !ok || strings.Contains(name, "/") || !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return host, nil
}

// fileInfo is a host file's info under its name in the mount.
type fileInfo struct {
	name string
	fs.FileInfo
}

func (i fileInfo) Name() string { return i.name }

type dirInfo struct{}

func (dirInfo) Name() string       { return "." }
func (dirInfo) Size() int64        { return 0 }
func (dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (dirInfo) ModTime() time.Time { return time.Time{} }
func (dirInfo) IsDir() bool        { return true }
func (dirInfo) Sys() any           { return nil }

// filesDir is the mount's root, opened.
type filesDir struct {
	f    Files
	read int // entries ReadDir has returned
}

func (d *filesDir) Stat() (fs.FileInfo, error) { return dirInfo{}, nil }
func (d *filesDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: ".", Err: fs.ErrInvalid}
}
func (d *filesDir) Close() error { return nil }
func (d *filesDir) ReadDir(n int) ([]fs.DirEntry, error) {
	list, err := d.f.ReadDir(".")
	if err != nil {
		return nil, err
	}
	list = list[min(d.read, len(list)):]
	if n > 0 {
		if len(list) == 0 {
			return nil, io.EOF
		}
		list = list[:min(n, len(list))]
	}
	d.read += len(list)
	return list, nil
}
