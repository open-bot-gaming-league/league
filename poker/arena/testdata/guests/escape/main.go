// Tries everything the sandbox forbids, at load and on its first decision,
// and reports each attempt on standard output: "denied <what>: <error>"
// when it was refused, "ESCAPED <what>" when it was not. It plays check or
// call.
package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
	"github.com/open-bot-gaming-league/league/poker/engine/sdk/wasmbot"
)

//go:wasmimport wasi_snapshot_preview1 path_open
func pathOpen(fd int32, dirflags uint32, path unsafe.Pointer, pathLen uint32, oflags uint32, base, inheriting uint64, fdflags uint32, out unsafe.Pointer) uint32

//go:wasmimport wasi_snapshot_preview1 sock_accept
func sockAccept(fd int32, flags uint32, out unsafe.Pointer) uint32

//go:wasmimport wasi_snapshot_preview1 fd_write
func fdWrite(fd int32, iovs unsafe.Pointer, n uint32, written unsafe.Pointer) uint32

func init() {
	wasmbot.Register(func() (sdk.Bot, error) {
		attempt("load")
		return &bot{}, nil
	})
}

type bot struct{ n int }

func (b *bot) Act(s sdk.State) sdk.Action {
	if b.n++; b.n == 1 {
		attempt("act")
	}
	return s.CheckOrCall()
}

func report(when, what string, err error) {
	if err == nil {
		fmt.Printf("ESCAPED %s %s\n", when, what)
		return
	}
	fmt.Printf("denied %s %s: %v\n", when, what, err)
}

func errno(e uint32) error {
	if e == 0 {
		return nil
	}
	return syscall.Errno(e)
}

func rawOpen(fd int32, path string, oflags uint32, rights uint64) error {
	p := []byte(path)
	var out int32
	return errno(pathOpen(fd, 0, unsafe.Pointer(&p[0]), uint32(len(p)), oflags, rights, rights, 0, unsafe.Pointer(&out)))
}

func attempt(when string) {
	// The one thing that must work: reading its own data.
	if b, err := os.ReadFile("/data/ok.txt"); err != nil || string(b) != "ok\n" {
		fmt.Printf("BROKEN %s read /data/ok.txt: %q %v\n", when, b, err)
	} else {
		fmt.Printf("allowed %s read /data/ok.txt\n", when)
	}
	for _, p := range []string{"/etc/passwd", "/data/../etc/passwd", "/proc/self/environ", "/data/out", "/data/sub/../../etc/passwd"} {
		_, err := os.ReadFile(p)
		report(when, "read "+p, err)
	}
	report(when, "raw open ../etc/passwd", rawOpen(3, "../etc/passwd", 0, 2))
	report(when, "raw open /etc/passwd", rawOpen(3, "/etc/passwd", 0, 2))
	report(when, "raw open sub/../../x", rawOpen(3, "sub/../../x", 0, 2))
	report(when, "raw open fd 4", rawOpen(4, "ok.txt", 0, 2))
	report(when, "raw open for write", rawOpen(3, "ok.txt", 0, 1<<6))
	report(when, "raw create", rawOpen(3, "new.txt", 1, 1<<6))
	report(when, "write /data/new.txt", os.WriteFile("/data/new.txt", []byte("x"), 0o644))
	_, err := os.OpenFile("/data/ok.txt", os.O_RDWR, 0)
	report(when, "open /data/ok.txt read-write", err)
	_, err = os.OpenFile("/data/ok.txt", os.O_WRONLY|os.O_APPEND, 0)
	report(when, "open /data/ok.txt append", err)
	report(when, "remove /data/ok.txt", os.Remove("/data/ok.txt"))
	report(when, "mkdir /data/d", os.Mkdir("/data/d", 0o755))
	report(when, "rename /data/ok.txt", os.Rename("/data/ok.txt", "/data/moved.txt"))
	report(when, "symlink /data/l", os.Symlink("/etc/passwd", "/data/l"))
	report(when, "write /tmp/x", os.WriteFile("/tmp/x", []byte("x"), 0o644))
	iov := [2]uint32{0, 4}
	var n uint32
	report(when, "raw write to the mount fd", errno(fdWrite(3, unsafe.Pointer(&iov[0]), 1, unsafe.Pointer(&n))))
	_, err = net.Dial("tcp", "127.0.0.1:80")
	report(when, "dial tcp", err)
	// Go's net on wasip1 is an in-process fake: a listener works, and
	// nothing outside the instance can reach it. Only its address is shown.
	if l, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		fmt.Printf("in-process %s listen tcp %s\n", when, l.Addr())
		l.Close()
	}
	var fd int32
	report(when, "raw sock_accept on the mount fd", errno(sockAccept(3, 0, unsafe.Pointer(&fd))))
	if env := os.Environ(); len(env) > 0 {
		fmt.Printf("ESCAPED %s environment %q\n", when, env)
	} else {
		fmt.Printf("denied %s environment: empty\n", when)
	}
	fmt.Printf("args %s %q\n", when, os.Args)
	if host, err := os.Hostname(); err == nil {
		fmt.Printf("hostname %s %q\n", when, host)
	}
	entries, err := os.ReadDir("/data")
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	fmt.Printf("listing %s /data %q %v\n", when, names, err)
	_, err = os.ReadDir("/")
	report(when, "list /", err)
	fmt.Printf("clock %s %s\n", when, time.Now().UTC().Format(time.RFC3339Nano))
}

func main() {}
