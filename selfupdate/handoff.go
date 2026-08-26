package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"syscall"
	"time"

	"binsync/release"
)

// restart hands the service over to the release installed at Path: it starts
// that binary with this process's listening sockets, waits for it to report
// itself serving, and then drains and ends this process. It returns only when
// the new process did not come up, with the previous binary back at Path
// (README 7, docs/DESIGN.md 6.3).
func (u *Updater) restart(h release.Hash) error {
	u.mu.Lock()
	switch {
	case u.superseded || u.upgrading:
		u.mu.Unlock()
		return errors.New("binsync: an upgrade is already in flight")
	case u.ctx.Err() != nil:
		u.mu.Unlock()
		return fmt.Errorf("binsync: not upgrading: %w", u.ctx.Err())
	}
	u.upgrading = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.upgrading = false
		u.mu.Unlock()
	}()

	if err := u.handoff(h); err != nil {
		return u.rollback(h, err)
	}
	return u.supersede()
}

// handoff starts the new binary on this process's sockets and waits for its
// Ready. Any outcome but that one leaves the child dead and returns why.
func (u *Updater) handoff(h release.Hash) error {
	files, specs, err := u.listenerFiles()
	if err != nil {
		return err
	}
	defer closeAll(files)

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("binsync: ready pipe: %w", err)
	}
	defer pr.Close()

	cmd, err := startExec(func() *exec.Cmd {
		// By path, never /proc/self/exe: this process runs the inode the
		// install replaced, and the point of the exec is to run the new one.
		cmd := exec.Command(u.path, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.ExtraFiles = append(files, pw) // fd 3 upwards, ready pipe last
		cmd.Env = append(os.Environ(),
			envFDs+"="+encodeFDs(specs),
			envReady+"="+strconv.Itoa(3+len(files)))
		return cmd
	})
	if err != nil {
		pw.Close()
		return fmt.Errorf("starting %s: %w", u.path, err)
	}
	// The write end must not stay open here, or the read below never sees
	// the EOF that means the new process died.
	pw.Close()
	u.log.Info("binsync: started the new release", "release", h, "path", u.path, "pid", cmd.Process.Pid, "listeners", len(files))

	var exitErr error
	waited := make(chan struct{})
	go func() { exitErr = cmd.Wait(); close(waited) }()
	ready := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(pr, b[:])
		ready <- err
	}()
	timer := time.NewTimer(u.readyTimeout)
	defer timer.Stop()

	var why error
	select {
	case err := <-ready:
		if err == nil {
			return nil
		}
		// The pipe closes as the new process dies, so its exit status is a
		// moment away and says more than the EOF does.
		select {
		case <-waited:
		case <-time.After(u.termTimeout):
			why = errors.New("the new process closed the ready pipe without reporting ready")
		}
	case <-waited:
	case <-timer.C:
		why = fmt.Errorf("the new process did not report ready within %s", u.readyTimeout)
	case <-u.ctx.Done():
		why = u.ctx.Err()
	}
	u.terminate(cmd, waited)
	if why == nil {
		why = errors.New("the new process exited before reporting ready")
		if exitErr != nil {
			why = fmt.Errorf("the new process failed: %w", exitErr)
		}
	}
	return why
}

// terminate ends a child that never reported ready: SIGTERM, then SIGKILL
// once termTimeout has passed.
func (u *Updater) terminate(cmd *exec.Cmd, waited <-chan struct{}) {
	select {
	case <-waited:
		return
	default:
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(u.termTimeout)
	defer timer.Stop()
	select {
	case <-waited:
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-waited
	}
}

// drainSettle is the pause between this process closing its listeners and its
// drain callbacks running. http.Server drops a connection whose request it
// finishes reading after Shutdown was called (the shuttingDown check in
// net/http's conn.serve), so the connections this process took off the socket
// a moment ago must reach their handler first. Twice the runtime's 10 ms
// preemption quantum covers a scheduler under load, out of a 30 s budget.
const drainSettle = 20 * time.Millisecond

// supersede is the point of no return: the new process is serving on the same
// sockets, so this one stops accepting, drains, and goes away.
func (u *Updater) supersede() error {
	u.mu.Lock()
	u.superseded = true
	serving, callbacks := slices.Clone(u.serving), slices.Clone(u.onShutdown)
	u.mu.Unlock()

	for _, s := range serving {
		// Closing our copy of a unix socket must not unlink the path the new
		// process is now serving on.
		if ul, ok := s.ln.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		s.ln.Close()
	}

	time.Sleep(drainSettle)

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for _, fn := range callbacks {
			fn()
		}
	}()
	timer := time.NewTimer(u.drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		u.log.Warn("binsync: shutdown callbacks did not finish", "after", u.drainTimeout)
	}

	u.log.Info("binsync: superseded")
	u.closeDone()
	u.exit(0)
	return nil
}

// rollback puts the previous binary back after a handoff that did not come up
// and records the release, so the loop skips it until the pointer names a
// different head (README guarantee 7).
func (u *Updater) rollback(h release.Hash, why error) error {
	u.log.Error("binsync: the new release did not come up; rolling back", "release", h, "err", why)
	// Record before reverting: a crash between the two must leave the release
	// skipped, not fetched and installed again.
	if err := u.inst.MarkFailed(h); err != nil {
		u.log.Error("binsync: recording the failed release", "err", err)
	}
	if err := u.inst.Revert(); err != nil {
		return fmt.Errorf("binsync: upgrade to %s failed: %w, and the revert failed: %w", h, why, err)
	}
	return fmt.Errorf("binsync: upgrade to %s rolled back: %w", h, why)
}

// listenerFiles duplicates every listener this process serves on, in Listen
// order, and describes them as the descriptors 3.. that the child will see.
func (u *Updater) listenerFiles() ([]*os.File, []fdSpec, error) {
	u.mu.Lock()
	serving := slices.Clone(u.serving)
	u.mu.Unlock()

	files := make([]*os.File, 0, len(serving))
	specs := make([]fdSpec, 0, len(serving))
	for i, s := range serving {
		f, err := dupListener(s.ln, fmt.Sprintf("binsync %s %s", s.network, s.addr))
		if err != nil {
			closeAll(files)
			return nil, nil, fmt.Errorf("binsync: handing over the %s listener on %s: %w", s.network, s.addr, err)
		}
		files = append(files, f)
		specs = append(specs, fdSpec{FD: 3 + i, Network: s.network, Addr: s.addr})
	}
	return files, specs, nil
}

// startExec retries the ETXTBSY that fork/exec reports while any thread of
// this process still holds the freshly installed file open for writing
// (golang.org/issue/22315). Each attempt needs its own Cmd, which is why this
// takes a builder: a Cmd refuses a second Start even after a failed one.
func startExec(build func() *exec.Cmd) (*exec.Cmd, error) {
	for wait := time.Millisecond; ; wait *= 4 {
		cmd := build()
		err := cmd.Start()
		if !errors.Is(err, syscall.ETXTBSY) || wait > 64*time.Millisecond {
			return cmd, err
		}
		time.Sleep(wait)
	}
}
