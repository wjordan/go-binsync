package selfupdate

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"syscall"
)

const (
	// envFDs carries the table of listeners across the handoff exec. Its
	// presence is also what tells a starting process that a handoff launched
	// it, and not a supervisor (docs/DESIGN.md 6.5).
	envFDs = "BINSYNC_FDS"
	// envReady names the descriptor the new process writes one byte to when
	// it is serving.
	envReady = "BINSYNC_READY"
)

// fdSpec is one inherited listener: the descriptor it arrives on, and the
// (network, address) pair the previous process asked Listen for, which is the
// key the new process looks it up by.
type fdSpec struct {
	FD      int    `json:"fd"`
	Network string `json:"net"`
	Addr    string `json:"addr"`
}

// inheritedFile is an fdSpec and the descriptor it names, until Listen claims
// it or Ready closes it unclaimed.
type inheritedFile struct {
	spec fdSpec
	f    *os.File
	used bool
}

// encodeFDs renders the table for envFDs. JSON, because a unix socket
// address is a path and can hold anything a filename can.
func encodeFDs(specs []fdSpec) string {
	b, _ := json.Marshal(specs) // a slice of fixed structs cannot fail
	return string(b)
}

func parseFDs(s string) ([]fdSpec, error) {
	var specs []fdSpec
	if err := json.Unmarshal([]byte(s), &specs); err != nil {
		return nil, fmt.Errorf("go-binsync: %s=%q: %w", envFDs, s, err)
	}
	for _, sp := range specs {
		// 0-2 are stdio: a listener never lands there, so a table naming one
		// is a table from something other than a go-binsync handoff.
		if sp.FD < 3 || sp.Network == "" {
			return nil, fmt.Errorf("go-binsync: %s=%q: %q on fd %d is not an inherited listener", envFDs, s, sp.Network, sp.FD)
		}
	}
	return specs, nil
}

// dupListener returns an inheritable duplicate of ln's socket.
//
// Not (*net.TCPListener).File: the file that returns reports itself to
// os/exec as one whose descriptor must be handed over in blocking mode, and
// clearing O_NONBLOCK there clears it on the open file description this
// process is still accepting on -- which parks the accepting thread in the
// kernel until the next connection arrives. os.NewFile over a descriptor that
// is already non-blocking leaves the flag alone.
func dupListener(ln net.Listener, name string) (*os.File, error) {
	sc, ok := ln.(syscall.Conn)
	if !ok {
		return nil, fmt.Errorf("a %T cannot be handed over", ln)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var dup int
	var dupErr error
	if err := raw.Control(func(fd uintptr) {
		// Close-on-exec: the duplicate must not leak into anything this
		// service starts between here and the handoff's own exec, which
		// clears the flag for the descriptors it passes on purpose.
		r, _, e := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_DUPFD_CLOEXEC, 0)
		if e != 0 {
			dupErr = os.NewSyscallError("fcntl", e)
			return
		}
		dup = int(r)
	}); err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	return os.NewFile(uintptr(dup), name), nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}
