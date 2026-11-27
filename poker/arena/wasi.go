package arena

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"math/bits"
	"math/rand/v2"
	"path"
	"strings"

	"github.com/bytecodealliance/wasmtime-go/v51"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot/abi"
)

// The WASI preview 1 the bots get: everything the sandbox allows, and a
// refusal for the rest (see the package doc). Each function is defined,
// so a module that imports one it never calls still loads.

const wasiModule = "wasi_snapshot_preview1"

// mount is where the bot's data is preopened.
const mount = "/data"

// WASI errno values (preview 1).
const (
	errnoSuccess     = 0
	errno2Big        = 1
	errnoAcces       = 2
	errnoBadf        = 8
	errnoFault       = 21
	errnoInval       = 28
	errnoIO          = 29
	errnoIsdir       = 31
	errnoMfile       = 33
	errnoNametoolong = 37
	errnoNoent       = 44
	errnoNosys       = 52
	errnoNotdir      = 54
	errnoNotsock     = 57
	errnoNotsup      = 58
	errnoRofs        = 69
	errnoSpipe       = 70
	errnoNotcapable  = 76
)

// File types, rights and flags.
const (
	fileCharDevice = 2
	fileDirectory  = 3
	fileRegular    = 4

	rightFdDatasync     = 1 << 0
	rightFdRead         = 1 << 1
	rightFdSeek         = 1 << 2
	rightFdWrite        = 1 << 6
	rightFdAllocate     = 1 << 8
	rightFdTell         = 1 << 5
	rightFdAdvise       = 1 << 7
	rightPathOpen       = 1 << 13
	rightFdReaddir      = 1 << 14
	rightPathFilestat   = 1 << 18
	rightFdFilestat     = 1 << 21
	rightPollReadwrite  = 1 << 27
	fileRights          = rightFdRead | rightFdSeek | rightFdTell | rightFdAdvise | rightFdFilestat | rightPollReadwrite
	dirRights           = rightPathOpen | rightFdReaddir | rightPathFilestat | rightFdFilestat
	writeRights         = rightFdDatasync | rightFdWrite | rightFdAllocate
	oflagCreat          = 1 << 0
	oflagDirectory      = 1 << 1
	oflagExcl           = 1 << 2
	oflagTrunc          = 1 << 3
	fdflagAppend        = 1 << 0
	eventClock          = 0
	eventFdRead         = 1
	eventFdWrite        = 2
	subscriptionAbstime = 1
	clockRealtime       = 0
	clockMonotonic      = 1
	clockProcessCPU     = 2
	clockThreadCPU      = 3
)

const (
	maxFDs   = 64
	maxIO    = 16 << 20 // bytes moved by one read or listing; more is a short read
	maxPath  = 4096
	maxSubs  = 1024
	epochNs  = 1_767_225_600_000_000_000 // the realtime clock's zero: 2026-01-01 00:00 UTC
	argv0    = "bot"
	maxSleep = 1 << 52 // ns: about 140 years
	maxLine  = 512
	iovecLen = 8
)

type fdKind int

const (
	kindStdin fdKind = iota
	kindStdout
	kindDir
	kindFile
)

type fdEntry struct {
	kind fdKind
	path string // in the mount: "." for /data itself
	file fs.File
	at   io.ReaderAt
	size int64
	pos  int64
	list []fs.DirEntry // read at the first fd_readdir
}

// sandbox is one bot's WASI state, the data of its wasmtime store.
type sandbox struct {
	store *wasmtime.Store
	mem   *wasmtime.Memory
	lim   Limits
	data  fs.FS
	fds   map[int32]*fdEntry
	rng   *rand.ChaCha8
	rand  *rand.Rand // the deciding State's Rand, for obgl.rand; nil outside a decision

	// Fuel, for the clock and the meter: spent before the current call,
	// and given to it.
	spent, given uint64
	slept        uint64 // ns of virtual sleep
	outOfFuel    bool   // a host call found too little fuel
	exited       *int32 // the code passed to proc_exit

	out              []byte // the first Limits.OutputBytes of the output
	line             []byte // the output's last line so far, up to maxLine bytes
	lastWords        string // the last line that was a Go runtime's "fatal error: " or "panic: "
	outBytes         int64
	readCalls, readB int64
}

func newSandbox(data fs.FS, lim Limits) *sandbox {
	s := &sandbox{lim: lim, data: data, fds: map[int32]*fdEntry{
		0: {kind: kindStdin}, 1: {kind: kindStdout}, 2: {kind: kindStdout},
	}}
	if data != nil {
		s.fds[3] = &fdEntry{kind: kindDir, path: "."}
	}
	// random_get is the same stream in every run: a bot's play draws on
	// State.Rand, and its runtime's hash seeds and the like stay replayable.
	var key [32]byte
	copy(key[:], "arena random_get")
	s.rng = rand.NewChaCha8(key)
	return s
}

func (s *sandbox) close() {
	for fd, e := range s.fds {
		if e.file != nil {
			e.file.Close()
		}
		delete(s.fds, fd)
	}
}

// memory is the guest's linear memory, valid until the call returns.
func (s *sandbox) memory(c *wasmtime.Caller) []byte {
	if s.mem == nil {
		if e := c.GetExport("memory"); e != nil {
			s.mem = e.Memory()
		}
		if s.mem == nil {
			return nil
		}
	}
	return s.mem.UnsafeData(c)
}

// span is n bytes of m at p, or false if they are not all in m.
func span(m []byte, p, n uint32) ([]byte, bool) {
	end := uint64(p) + uint64(n)
	if end > uint64(len(m)) {
		return nil, false
	}
	return m[p:end], true
}

func put32(m []byte, p uint32, v uint32) bool {
	b, ok := span(m, p, 4)
	if ok {
		binary.LittleEndian.PutUint32(b, v)
	}
	return ok
}

func put64(m []byte, p uint32, v uint64) bool {
	b, ok := span(m, p, 8)
	if ok {
		binary.LittleEndian.PutUint64(b, v)
	}
	return ok
}

// charge takes calls*CallFuel + bytes*ByteFuel from the store's fuel, or
// traps the bot, out of fuel, if it has less.
func (s *sandbox) charge(bytes uint64) *wasmtime.Trap {
	hi, perBytes := bits.Mul64(bytes, s.lim.ByteFuel)
	cost, carry := bits.Add64(perBytes, s.lim.CallFuel, 0)
	left, err := s.store.GetFuel()
	if err != nil {
		return wasmtime.NewTrap("arena: " + err.Error())
	}
	if hi != 0 || carry != 0 || cost > left {
		s.outOfFuel = true
		s.store.SetFuel(0)
		return wasmtime.NewTrap("out of fuel in a host call")
	}
	if err := s.store.SetFuel(left - cost); err != nil {
		return wasmtime.NewTrap("arena: " + err.Error())
	}
	return nil
}

// fuelSpent is the fuel the bot has burnt since it was loaded.
func (s *sandbox) fuelSpent() uint64 {
	left, _ := s.store.GetFuel()
	return s.spent + (s.given - min(left, s.given))
}

// now is the virtual monotonic clock, in ns.
func (s *sandbox) now() uint64 { return s.fuelSpent() + s.slept }

// scanLines follows the output line by line for lastWords: a Go runtime
// writes its abort message in pieces, then a stack trace that may run long.
func (s *sandbox) scanLines(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.line = append(s.line, b[:min(len(b), maxLine-len(s.line))]...)
			return
		}
		s.line = append(s.line, b[:min(i, maxLine-len(s.line))]...)
		if l := string(s.line); strings.HasPrefix(l, "fatal error: ") || strings.HasPrefix(l, "panic: ") {
			s.lastWords = l
		}
		s.line, b = s.line[:0], b[i+1:]
	}
}

func state(c *wasmtime.Caller) *sandbox { return c.Data().(*sandbox) }

type trap = *wasmtime.Trap

// wasiFuncs is every WASI preview 1 function. Each takes the caller first
// and returns an errno and a trap (nil to return normally).
var wasiFuncs = map[string]any{
	"args_sizes_get": func(c *wasmtime.Caller, argc, size int32) (int32, trap) {
		return state(c).call(c, func(m []byte) int32 {
			return ok(put32(m, uint32(argc), 1) && put32(m, uint32(size), uint32(len(argv0)+1)))
		})
	},
	"args_get": func(c *wasmtime.Caller, argv, buf int32) (int32, trap) {
		return state(c).call(c, func(m []byte) int32 {
			b, fits := span(m, uint32(buf), uint32(len(argv0)+1))
			if !fits || !put32(m, uint32(argv), uint32(buf)) {
				return errnoFault
			}
			copy(b, argv0+"\x00")
			return errnoSuccess
		})
	},
	"environ_sizes_get": func(c *wasmtime.Caller, count, size int32) (int32, trap) {
		return state(c).call(c, func(m []byte) int32 { return ok(put32(m, uint32(count), 0) && put32(m, uint32(size), 0)) })
	},
	"environ_get": func(c *wasmtime.Caller, env, buf int32) (int32, trap) {
		return state(c).call(c, func([]byte) int32 { return errnoSuccess })
	},
	"clock_res_get": func(c *wasmtime.Caller, id, res int32) (int32, trap) {
		return state(c).call(c, func(m []byte) int32 {
			if id < clockRealtime || id > clockThreadCPU {
				return errnoInval
			}
			return ok(put64(m, uint32(res), 1))
		})
	},
	"clock_time_get": func(c *wasmtime.Caller, id int32, precision int64, at int32) (int32, trap) {
		s := state(c)
		return s.call(c, func(m []byte) int32 {
			t := s.now()
			switch id {
			case clockRealtime:
				t += epochNs
			case clockMonotonic, clockProcessCPU, clockThreadCPU:
			default:
				return errnoInval
			}
			return ok(put64(m, uint32(at), t))
		})
	},
	"fd_advise": func(c *wasmtime.Caller, fd int32, offset, length int64, advice int32) (int32, trap) {
		s := state(c)
		return s.call(c, func([]byte) int32 { return s.fileOnly(fd, errnoSuccess) })
	},
	"fd_allocate": func(c *wasmtime.Caller, fd int32, offset, length int64) (int32, trap) {
		return state(c).deny(c, errnoRofs)
	},
	"fd_close": func(c *wasmtime.Caller, fd int32) (int32, trap) {
		s := state(c)
		return s.call(c, func([]byte) int32 {
			e := s.fds[fd]
			if e == nil {
				return errnoBadf
			}
			if e.file != nil {
				e.file.Close()
			}
			delete(s.fds, fd)
			return errnoSuccess
		})
	},
	"fd_datasync":         func(c *wasmtime.Caller, fd int32) (int32, trap) { return state(c).deny(c, errnoBadf) },
	"fd_sync":             func(c *wasmtime.Caller, fd int32) (int32, trap) { return state(c).deny(c, errnoBadf) },
	"fd_fdstat_get":       func(c *wasmtime.Caller, fd, at int32) (int32, trap) { return state(c).fdstat(c, fd, at) },
	"fd_fdstat_set_flags": func(c *wasmtime.Caller, fd, flags int32) (int32, trap) { return state(c).setFlags(c, fd, flags) },
	"fd_fdstat_set_rights": func(c *wasmtime.Caller, fd int32, base, inh int64) (int32, trap) {
		return state(c).deny(c, errnoNotsup)
	},
	"fd_filestat_get":      func(c *wasmtime.Caller, fd, at int32) (int32, trap) { return state(c).fdFilestat(c, fd, at) },
	"fd_filestat_set_size": func(c *wasmtime.Caller, fd int32, size int64) (int32, trap) { return state(c).deny(c, errnoRofs) },
	"fd_filestat_set_times": func(c *wasmtime.Caller, fd int32, atim, mtim int64, flags int32) (int32, trap) {
		return state(c).deny(c, errnoRofs)
	},
	"fd_pread": func(c *wasmtime.Caller, fd, iovs, n int32, offset int64, nread int32) (int32, trap) {
		return state(c).read(c, fd, iovs, n, offset, nread)
	},
	"fd_read": func(c *wasmtime.Caller, fd, iovs, n, nread int32) (int32, trap) {
		return state(c).read(c, fd, iovs, n, -1, nread)
	},
	"fd_prestat_get":      func(c *wasmtime.Caller, fd, at int32) (int32, trap) { return state(c).prestat(c, fd, at) },
	"fd_prestat_dir_name": func(c *wasmtime.Caller, fd, at, n int32) (int32, trap) { return state(c).prestatName(c, fd, at, n) },
	"fd_pwrite": func(c *wasmtime.Caller, fd, iovs, n int32, offset int64, nwritten int32) (int32, trap) {
		return state(c).deny(c, errnoBadf)
	},
	"fd_readdir": func(c *wasmtime.Caller, fd, buf, n int32, cookie int64, used int32) (int32, trap) {
		return state(c).readdir(c, fd, buf, n, cookie, used)
	},
	"fd_renumber": func(c *wasmtime.Caller, fd, to int32) (int32, trap) { return state(c).deny(c, errnoNotsup) },
	"fd_seek": func(c *wasmtime.Caller, fd int32, offset int64, whence, at int32) (int32, trap) {
		return state(c).seek(c, fd, offset, whence, at)
	},
	"fd_tell": func(c *wasmtime.Caller, fd, at int32) (int32, trap) {
		return state(c).seek(c, fd, 0, io.SeekCurrent, at)
	},
	"fd_write": func(c *wasmtime.Caller, fd, iovs, n, nwritten int32) (int32, trap) {
		return state(c).write(c, fd, iovs, n, nwritten)
	},
	"path_create_directory": func(c *wasmtime.Caller, fd, p, n int32) (int32, trap) { return state(c).deny(c, errnoRofs) },
	"path_filestat_get": func(c *wasmtime.Caller, fd, flags, p, n, at int32) (int32, trap) {
		return state(c).pathFilestat(c, fd, p, n, at)
	},
	"path_filestat_set_times": func(c *wasmtime.Caller, fd, flags, p, n int32, atim, mtim int64, fst int32) (int32, trap) {
		return state(c).deny(c, errnoRofs)
	},
	"path_link": func(c *wasmtime.Caller, fd, flags, p, n, fd2, p2, n2 int32) (int32, trap) {
		return state(c).deny(c, errnoRofs)
	},
	"path_open": func(c *wasmtime.Caller, fd, dirflags, p, n, oflags int32, base, inh int64, fdflags, at int32) (int32, trap) {
		return state(c).open(c, fd, p, n, oflags, base, fdflags, at)
	},
	"path_readlink": func(c *wasmtime.Caller, fd, p, n, buf, bufLen, used int32) (int32, trap) {
		return state(c).deny(c, errnoInval) // the mount shows no symlinks
	},
	"path_remove_directory": func(c *wasmtime.Caller, fd, p, n int32) (int32, trap) { return state(c).deny(c, errnoRofs) },
	"path_rename": func(c *wasmtime.Caller, fd, p, n, fd2, p2, n2 int32) (int32, trap) {
		return state(c).deny(c, errnoRofs)
	},
	"path_symlink": func(c *wasmtime.Caller, p, n, fd, p2, n2 int32) (int32, trap) {
		return state(c).deny(c, errnoRofs)
	},
	"path_unlink_file": func(c *wasmtime.Caller, fd, p, n int32) (int32, trap) { return state(c).deny(c, errnoRofs) },
	"poll_oneoff": func(c *wasmtime.Caller, in, out, n, nevents int32) (int32, trap) {
		return state(c).poll(c, in, out, n, nevents)
	},
	"proc_exit": func(c *wasmtime.Caller, code int32) trap {
		state(c).exited = &code
		return wasmtime.NewTrap("exit")
	},
	"proc_raise":  func(c *wasmtime.Caller, sig int32) (int32, trap) { return state(c).deny(c, errnoNosys) },
	"sched_yield": func(c *wasmtime.Caller) (int32, trap) { return state(c).deny(c, errnoSuccess) },
	"random_get": func(c *wasmtime.Caller, buf, n int32) (int32, trap) {
		s := state(c)
		if t := s.charge(uint64(uint32(n))); t != nil {
			return 0, t
		}
		b, fits := span(s.memory(c), uint32(buf), uint32(n))
		if !fits {
			return errnoFault, nil
		}
		s.rng.Read(b)
		return errnoSuccess, nil
	},
	"sock_accept": func(c *wasmtime.Caller, fd, flags, at int32) (int32, trap) { return state(c).deny(c, errnoBadf) },
	"sock_recv": func(c *wasmtime.Caller, fd, iovs, n, flags, nread, oflags int32) (int32, trap) {
		return state(c).deny(c, errnoNotsock)
	},
	"sock_send": func(c *wasmtime.Caller, fd, iovs, n, flags, nwritten int32) (int32, trap) {
		return state(c).deny(c, errnoNotsock)
	},
	"sock_shutdown": func(c *wasmtime.Caller, fd, how int32) (int32, trap) { return state(c).deny(c, errnoNotsock) },
}

// defineImports defines WASI and obgl.rand.
func defineImports(l *wasmtime.Linker) error {
	for name, f := range wasiFuncs {
		if err := l.FuncWrap(wasiModule, name, f); err != nil {
			return err
		}
	}
	return l.FuncWrap(abi.ImportModule, abi.ImportRand, func(c *wasmtime.Caller, dst, n int32) (int32, trap) {
		return state(c).drawRand(c, dst, n)
	})
}

// drawRand is obgl.rand: the next n words of the deciding State's Rand.
// It answers 1 outside a decision, for a State without one, or for a bad
// request.
func (s *sandbox) drawRand(c *wasmtime.Caller, dst, n int32) (int32, trap) {
	if s.rand == nil || n < 0 || n > abi.MaxRand {
		return s.deny(c, 1)
	}
	b, fits := span(s.memory(c), uint32(dst), 8*uint32(n))
	if !fits {
		return s.deny(c, 1)
	}
	if t := s.charge(8 * uint64(n)); t != nil {
		return 0, t
	}
	for i := range int(n) {
		binary.LittleEndian.PutUint64(b[8*i:], s.rand.Uint64())
	}
	return 0, nil
}

func ok(fits bool) int32 {
	if fits {
		return errnoSuccess
	}
	return errnoFault
}

// call charges a call's CallFuel and runs f on the guest's memory.
func (s *sandbox) call(c *wasmtime.Caller, f func(m []byte) int32) (int32, trap) {
	if t := s.charge(0); t != nil {
		return 0, t
	}
	m := s.memory(c)
	if m == nil {
		return errnoFault, nil
	}
	return f(m), nil
}

// deny charges a call's CallFuel and answers errno.
func (s *sandbox) deny(c *wasmtime.Caller, errno int32) (int32, trap) {
	if t := s.charge(0); t != nil {
		return 0, t
	}
	return errno, nil
}

func (s *sandbox) fileOnly(fd int32, errno int32) int32 {
	switch e := s.fds[fd]; {
	case e == nil:
		return errnoBadf
	case e.kind != kindFile:
		return errnoBadf
	}
	return errno
}

func (s *sandbox) setFlags(c *wasmtime.Caller, fd, flags int32) (int32, trap) {
	return s.call(c, func([]byte) int32 {
		switch {
		case s.fds[fd] == nil:
			return errnoBadf
		case flags&fdflagAppend != 0:
			return errnoRofs
		}
		return errnoSuccess // nonblocking or not, nothing here ever blocks
	})
}

func (s *sandbox) fdstat(c *wasmtime.Caller, fd, at int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		e := s.fds[fd]
		if e == nil {
			return errnoBadf
		}
		b, fits := span(m, uint32(at), 24)
		if !fits {
			return errnoFault
		}
		clear(b)
		var ftype byte
		var base, inh uint64
		switch e.kind {
		case kindStdin:
			ftype, base = fileCharDevice, rightFdRead|rightPollReadwrite
		case kindStdout:
			ftype, base = fileCharDevice, rightFdWrite|rightPollReadwrite
		case kindDir:
			ftype, base, inh = fileDirectory, dirRights, fileRights|dirRights
		case kindFile:
			ftype, base = fileRegular, fileRights
		}
		b[0] = ftype
		binary.LittleEndian.PutUint64(b[8:], base)
		binary.LittleEndian.PutUint64(b[16:], inh)
		return errnoSuccess
	})
}

// filestat writes a WASI filestat: what the mount shows of a file is its
// type and size, and nothing that differs between machines.
func filestat(m []byte, at uint32, ftype byte, size int64) int32 {
	b, fits := span(m, at, 64)
	if !fits {
		return errnoFault
	}
	clear(b)
	b[16] = ftype
	binary.LittleEndian.PutUint64(b[24:], 1)
	binary.LittleEndian.PutUint64(b[32:], uint64(size))
	return errnoSuccess
}

func (s *sandbox) fdFilestat(c *wasmtime.Caller, fd, at int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		e := s.fds[fd]
		switch {
		case e == nil:
			return errnoBadf
		case e.kind == kindDir:
			return filestat(m, uint32(at), fileDirectory, 0)
		case e.kind == kindFile:
			return filestat(m, uint32(at), fileRegular, e.size)
		}
		return filestat(m, uint32(at), fileCharDevice, 0)
	})
}

func (s *sandbox) prestat(c *wasmtime.Caller, fd, at int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		if fd != 3 || s.data == nil {
			return errnoBadf
		}
		b, fits := span(m, uint32(at), 8)
		if !fits {
			return errnoFault
		}
		clear(b)
		binary.LittleEndian.PutUint32(b[4:], uint32(len(mount)))
		return errnoSuccess
	})
}

func (s *sandbox) prestatName(c *wasmtime.Caller, fd, at, n int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		if fd != 3 || s.data == nil {
			return errnoBadf
		}
		if uint32(n) < uint32(len(mount)) {
			return errnoNametoolong
		}
		b, fits := span(m, uint32(at), uint32(len(mount)))
		if !fits {
			return errnoFault
		}
		copy(b, mount)
		return errnoSuccess
	})
}

// resolve is a guest path relative to directory fd as a path in the
// mount. Absolute paths and paths that climb out of the mount are not
// capable: there is nothing outside it.
func (s *sandbox) resolve(m []byte, fd, p, n int32) (string, int32) {
	e := s.fds[fd]
	switch {
	case e == nil:
		return "", errnoBadf
	case e.kind != kindDir:
		return "", errnoNotdir
	case uint32(n) > maxPath:
		return "", errnoNametoolong
	}
	b, fits := span(m, uint32(p), uint32(n))
	if !fits {
		return "", errnoFault
	}
	rel := string(b)
	if strings.HasPrefix(rel, "/") {
		return "", errnoNotcapable
	}
	full := path.Join(e.path, rel)
	if !fs.ValidPath(full) {
		return "", errnoNotcapable
	}
	return full, errnoSuccess
}

// fsErrno maps an fs error to an errno. Anything unexpected, such as a
// symlink out of the mount, is refused as not permitted.
func fsErrno(err error) int32 {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errnoNoent
	case errors.Is(err, fs.ErrInvalid):
		return errnoInval
	}
	return errnoAcces
}

func (s *sandbox) pathFilestat(c *wasmtime.Caller, fd, p, n, at int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		name, errno := s.resolve(m, fd, p, n)
		if errno != errnoSuccess {
			return errno
		}
		info, err := fs.Stat(s.data, name)
		if err != nil {
			return fsErrno(err)
		}
		if info.IsDir() {
			return filestat(m, uint32(at), fileDirectory, 0)
		}
		if !info.Mode().IsRegular() {
			return errnoAcces
		}
		return filestat(m, uint32(at), fileRegular, info.Size())
	})
}

func (s *sandbox) open(c *wasmtime.Caller, fd, p, n, oflags int32, base int64, fdflags, at int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		name, errno := s.resolve(m, fd, p, n)
		if errno != errnoSuccess {
			return errno
		}
		if oflags&(oflagCreat|oflagExcl|oflagTrunc) != 0 || fdflags&fdflagAppend != 0 || uint64(base)&writeRights != 0 {
			return errnoRofs
		}
		free := int32(-1)
		for i := int32(3); i < maxFDs; i++ {
			if s.fds[i] == nil {
				free = i
				break
			}
		}
		if free < 0 {
			return errnoMfile
		}
		info, err := fs.Stat(s.data, name)
		if err != nil {
			return fsErrno(err)
		}
		var e *fdEntry
		switch {
		case info.IsDir():
			e = &fdEntry{kind: kindDir, path: name}
		case oflags&oflagDirectory != 0:
			return errnoNotdir
		case !info.Mode().IsRegular():
			return errnoAcces
		default:
			f, err := s.data.Open(name)
			if err != nil {
				return fsErrno(err)
			}
			ra, isRA := f.(io.ReaderAt)
			if !isRA {
				f.Close()
				return errnoIO // Config.Data must give files with ReadAt
			}
			e = &fdEntry{kind: kindFile, path: name, file: f, at: ra, size: info.Size()}
		}
		if !put32(m, uint32(at), uint32(free)) {
			if e.file != nil {
				e.file.Close()
			}
			return errnoFault
		}
		s.fds[free] = e
		return errnoSuccess
	})
}

// iovecs is the guest's buffers named by n iovecs at iovs.
func iovecs(m []byte, iovs, n int32) ([][]byte, int32) {
	if uint32(n) > 1024 {
		return nil, errnoInval
	}
	out := make([][]byte, 0, n)
	for i := range uint32(n) {
		v, fits := span(m, uint32(iovs)+i*iovecLen, iovecLen)
		if !fits {
			return nil, errnoFault
		}
		b, fits := span(m, binary.LittleEndian.Uint32(v), binary.LittleEndian.Uint32(v[4:]))
		if !fits {
			return nil, errnoFault
		}
		out = append(out, b)
	}
	return out, errnoSuccess
}

// read is fd_read (offset < 0: at the file's position, which it moves) and
// fd_pread. One call moves at most maxIO bytes.
func (s *sandbox) read(c *wasmtime.Caller, fd, iovs, n int32, offset int64, nread int32) (int32, trap) {
	e := s.fds[fd]
	if e == nil || e.kind == kindStdout || e.kind == kindDir {
		code := int32(errnoBadf)
		if e != nil && e.kind == kindDir {
			code = errnoIsdir
		}
		return s.deny(c, code)
	}
	m := s.memory(c)
	bufs, errno := iovecs(m, iovs, n)
	if errno != errnoSuccess {
		return s.deny(c, errno)
	}
	if e.kind == kindStdin {
		return s.call(c, func(m []byte) int32 { return ok(put32(m, uint32(nread), 0)) }) // empty
	}
	at := offset
	if offset < 0 {
		at = e.pos
	}
	want := int64(0)
	for _, b := range bufs {
		want += int64(len(b))
	}
	want = max(0, min(want, maxIO, e.size-at))
	if t := s.charge(uint64(want)); t != nil {
		return 0, t
	}
	s.readCalls++
	got := int64(0)
	for _, b := range bufs {
		if got == want {
			break
		}
		b = b[:min(int64(len(b)), want-got)]
		k, err := e.at.ReadAt(b, at+got)
		got += int64(k)
		if err != nil && !(errors.Is(err, io.EOF) && k == len(b)) {
			if errors.Is(err, io.EOF) {
				break
			}
			return errnoIO, nil
		}
	}
	s.readB += got
	if offset < 0 {
		e.pos += got
	}
	return ok(put32(m, uint32(nread), uint32(got))), nil
}

func (s *sandbox) seek(c *wasmtime.Caller, fd int32, offset int64, whence, at int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		e := s.fds[fd]
		switch {
		case e == nil:
			return errnoBadf
		case e.kind != kindFile:
			return errnoSpipe
		}
		var base int64
		switch whence {
		case io.SeekStart:
		case io.SeekCurrent:
			base = e.pos
		case io.SeekEnd:
			base = e.size
		default:
			return errnoInval
		}
		pos := base + offset
		if (offset > 0 && pos < base) || pos < 0 {
			return errnoInval
		}
		if !put64(m, uint32(at), uint64(pos)) {
			return errnoFault
		}
		e.pos = pos
		return errnoSuccess
	})
}

// write is fd_write: standard output and error are kept up to the
// Limits' OutputBytes; nothing else is writable.
func (s *sandbox) write(c *wasmtime.Caller, fd, iovs, n, nwritten int32) (int32, trap) {
	e := s.fds[fd]
	if e == nil || e.kind != kindStdout {
		return s.deny(c, errnoBadf)
	}
	m := s.memory(c)
	bufs, errno := iovecs(m, iovs, n)
	if errno != errnoSuccess {
		return s.deny(c, errno)
	}
	total := uint64(0)
	for _, b := range bufs {
		total += uint64(len(b))
	}
	if total > 1<<31 {
		return s.deny(c, errnoInval)
	}
	if t := s.charge(total); t != nil {
		return 0, t
	}
	for _, b := range bufs {
		if room := s.lim.OutputBytes - len(s.out); room > 0 {
			s.out = append(s.out, b[:min(len(b), room)]...)
		}
		s.scanLines(b)
	}
	s.outBytes += int64(total)
	return ok(put32(m, uint32(nwritten), uint32(total))), nil
}

// readdir is fd_readdir: the directory's entries in name order, cookie
// being the index of the next.
func (s *sandbox) readdir(c *wasmtime.Caller, fd, buf, n int32, cookie int64, used int32) (int32, trap) {
	e := s.fds[fd]
	if e == nil || e.kind != kindDir {
		code := int32(errnoBadf)
		if e != nil {
			code = errnoNotdir
		}
		return s.deny(c, code)
	}
	if e.list == nil {
		list, err := fs.ReadDir(s.data, e.path)
		if err != nil {
			return s.deny(c, fsErrno(err))
		}
		e.list = list
	}
	size := min(uint32(n), maxIO)
	m := s.memory(c)
	out, fits := span(m, uint32(buf), size)
	if !fits {
		return s.deny(c, errnoFault)
	}
	if t := s.charge(uint64(size)); t != nil {
		return 0, t
	}
	w := 0
	for i := max(cookie, 0); i < int64(len(e.list)) && w < len(out); i++ {
		d := e.list[i]
		ent := make([]byte, 24+len(d.Name()))
		binary.LittleEndian.PutUint64(ent[0:], uint64(i+1))
		binary.LittleEndian.PutUint32(ent[16:], uint32(len(d.Name())))
		ent[20] = fileRegular
		if d.IsDir() {
			ent[20] = fileDirectory
		}
		copy(ent[24:], d.Name())
		w += copy(out[w:], ent) // a partial last entry tells the caller to come back for it
	}
	return ok(put32(m, uint32(used), uint32(w))), nil
}

// poll is poll_oneoff. Descriptors are always ready, so a poll with any
// returns at once; a poll on clocks alone sleeps, in virtual time, until
// the first of them.
func (s *sandbox) poll(c *wasmtime.Caller, in, out, n, nevents int32) (int32, trap) {
	return s.call(c, func(m []byte) int32 {
		if n <= 0 || n > maxSubs {
			return errnoInval
		}
		subs, fits := span(m, uint32(in), uint32(n)*48)
		if !fits {
			return errnoFault
		}
		evs, fits := span(m, uint32(out), uint32(n)*32)
		if !fits {
			return errnoFault
		}
		clear(evs)
		now := s.now()
		count := 0
		event := func(sub []byte, kind byte, errno int32, nbytes uint64) {
			ev := evs[32*count:]
			copy(ev[0:8], sub[0:8])
			binary.LittleEndian.PutUint16(ev[8:], uint16(errno))
			ev[10] = kind
			binary.LittleEndian.PutUint64(ev[16:], nbytes)
			count++
		}
		wake := ^uint64(0)
		for i := range int(n) {
			sub := subs[48*i : 48*i+48]
			switch kind := sub[8]; kind {
			case eventFdRead, eventFdWrite:
				e := s.fds[int32(binary.LittleEndian.Uint32(sub[16:]))]
				switch {
				case e == nil:
					event(sub, kind, errnoBadf, 0)
				case e.kind == kindFile:
					event(sub, kind, errnoSuccess, uint64(max(0, e.size-e.pos)))
				default:
					event(sub, kind, errnoSuccess, 0)
				}
			case eventClock:
				wake = min(wake, s.deadline(sub, now))
			default:
				event(sub, kind, errnoInval, 0)
			}
		}
		if count == 0 {
			if wake > now {
				s.slept += min(wake-now, maxSleep) // the clock stays far from overflowing
			}
			for i := range int(n) {
				if sub := subs[48*i : 48*i+48]; sub[8] == eventClock && s.deadline(sub, now) <= wake {
					event(sub, eventClock, errnoSuccess, 0)
				}
			}
		}
		return ok(put32(m, uint32(nevents), uint32(count)))
	})
}

// deadline is when a clock subscription fires, on the monotonic clock.
func (s *sandbox) deadline(sub []byte, now uint64) uint64 {
	id := binary.LittleEndian.Uint32(sub[16:])
	timeout := binary.LittleEndian.Uint64(sub[24:])
	if binary.LittleEndian.Uint16(sub[40:])&subscriptionAbstime == 0 {
		if t := now + timeout; t >= now {
			return t
		}
		return ^uint64(0)
	}
	if id == clockRealtime {
		if timeout < epochNs {
			return 0
		}
		return timeout - epochNs
	}
	return timeout
}
