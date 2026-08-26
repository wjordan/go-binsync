package selfupdate

import (
	"errors"
	"net"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestFDTableRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		specs []fdSpec
	}{
		{"none", nil},
		{"one tcp listener", []fdSpec{{3, "tcp", ":8080"}}},
		{"several", []fdSpec{{3, "tcp", "127.0.0.1:0"}, {4, "tcp6", "[::1]:9000"}, {5, "unix", "/run/app.sock"}}},
		{"an address that would break a delimited encoding", []fdSpec{{3, "unix", "/tmp/a,b:c=d e\n.sock"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFDs(encodeFDs(tc.specs))
			if err != nil {
				t.Fatalf("parseFDs(%q): %v", encodeFDs(tc.specs), err)
			}
			if len(got) == 0 && len(tc.specs) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.specs) {
				t.Errorf("round trip = %v, want %v", got, tc.specs)
			}
		})
	}
}

func TestFDTableRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, env string }{
		{"not json", "3:tcp::8080"},
		{"stdio", `[{"fd":2,"net":"tcp","addr":":80"}]`},
		{"no network", `[{"fd":3,"net":"","addr":":80"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseFDs(tc.env); err == nil {
				t.Errorf("parseFDs(%q) = nil error, want one", tc.env)
			}
		})
	}
}

// The descriptor handed to a child must not disturb the socket this process
// is still accepting on: os/exec takes each ExtraFiles entry through
// os.File.Fd, which is where (*net.TCPListener).File would lose O_NONBLOCK.
func TestDupListenerKeepsTheListenerNonBlocking(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	f, err := dupListener(ln, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = f.Fd() // what os/exec does with it

	if !nonblocking(t, ln) {
		t.Error("the listener lost O_NONBLOCK to the duplicate handed to the child")
	}
	// An Accept must still return promptly rather than park in the kernel.
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := ln.Accept(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Accept = %v, want a deadline error", err)
	}
}

func nonblocking(t *testing.T, ln net.Listener) bool {
	t.Helper()
	raw, err := ln.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	if err := raw.Control(func(fd uintptr) {
		f, _, e := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		if e != 0 {
			t.Fatalf("fcntl: %v", e)
		}
		flags = int(f)
	}); err != nil {
		t.Fatal(err)
	}
	return flags&syscall.O_NONBLOCK != 0
}
