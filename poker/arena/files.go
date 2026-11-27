package arena

import (
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// Files is a data mount made of host files: each name in it is a file of
// the bot's /data, read from the host path it maps to, and the folders
// above the names (a/b.bin puts a file b.bin in a folder a) are made up
// from them. It is how one set of files on the host serves several bots,
// each seeing only its own: the house bots' bucket files, or a submitted
// bot's files kept under their sha256. A name is a valid fs.ValidPath
// other than "."; a name that is also a folder of another name is a file.
type Files map[string]string

var (
	_ fs.ReadDirFS = Files(nil)
	_ fs.StatFS    = Files(nil)
)

func (f Files) Open(name string) (fs.File, error) {
	if host, ok := f.file(name); ok {
		file, err := os.Open(host)
		if err != nil {
			return nil, err
		}
		return mountedFile{File: file, name: path.Base(name)}, nil
	}
	if !f.dir(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &filesDir{f: f, name: name}, nil
}

func (f Files) Stat(name string) (fs.FileInfo, error) {
	if host, ok := f.file(name); ok {
		info, err := os.Stat(host)
		if err != nil {
			return nil, err
		}
		return fileInfo{name: path.Base(name), FileInfo: info}, nil
	}
	if !f.dir(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return dirInfo{name: path.Base(name)}, nil
}

// ReadDir is the folder's entries, sorted by name: its files and the
// folders under it.
func (f Files) ReadDir(name string) ([]fs.DirEntry, error) {
	if _, ok := f.file(name); ok || !f.dir(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}
	children := map[string]bool{}
	for n := range f {
		if rest, ok := strings.CutPrefix(n, prefix); ok && fs.ValidPath(n) && n != "." {
			child, _, _ := strings.Cut(rest, "/")
			children[child] = true
		}
	}
	var out []fs.DirEntry
	for _, child := range slices.Sorted(func(yield func(string) bool) {
		for c := range children {
			if !yield(c) {
				return
			}
		}
	}) {
		info, err := f.Stat(prefix + child)
		if err != nil {
			return nil, err
		}
		out = append(out, fs.FileInfoToDirEntry(info))
	}
	return out, nil
}

// file is the host path of the file name, if name is one.
func (f Files) file(name string) (string, bool) {
	host, ok := f[name]
	return host, ok && name != "." && fs.ValidPath(name)
}

// dir reports whether name is the mount's root or a folder above a file.
func (f Files) dir(name string) bool {
	if name == "." {
		return true
	}
	if !fs.ValidPath(name) {
		return false
	}
	for n := range f {
		if strings.HasPrefix(n, name+"/") && fs.ValidPath(n) {
			return true
		}
	}
	return false
}

// mountedFile is a host file, opened, under its name in the mount.
type mountedFile struct {
	*os.File
	name string
}

func (m mountedFile) Stat() (fs.FileInfo, error) {
	info, err := m.File.Stat()
	if err != nil {
		return nil, err
	}
	return fileInfo{name: m.name, FileInfo: info}, nil
}

// fileInfo is a host file's info under its name in the mount.
type fileInfo struct {
	name string
	fs.FileInfo
}

func (i fileInfo) Name() string { return i.name }

// dirInfo is a made-up folder's info.
type dirInfo struct{ name string }

func (d dirInfo) Name() string     { return d.name }
func (dirInfo) Size() int64        { return 0 }
func (dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (dirInfo) ModTime() time.Time { return time.Time{} }
func (dirInfo) IsDir() bool        { return true }
func (dirInfo) Sys() any           { return nil }

// filesDir is a folder of the mount, opened.
type filesDir struct {
	f    Files
	name string
	read int // entries ReadDir has returned
}

func (d *filesDir) Stat() (fs.FileInfo, error) { return dirInfo{name: path.Base(d.name)}, nil }
func (d *filesDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}
func (d *filesDir) Close() error { return nil }
func (d *filesDir) ReadDir(n int) ([]fs.DirEntry, error) {
	list, err := d.f.ReadDir(d.name)
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
