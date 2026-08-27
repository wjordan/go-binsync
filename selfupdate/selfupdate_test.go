package selfupdate

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wjordan/go-binsync/release"
)

// testUpdater builds an Updater whose two process-ending calls are inert and
// whose timeouts are short enough for a test, so the whole lifecycle runs
// inside the one test process. The seams are in place before start, which is
// where the exec of the crash check happens.
func testUpdater(t *testing.T, path string, src updateSource) (*Updater, *string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	u := newUpdater(Config{
		Path:    path,
		Context: ctx,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, src)
	execed := new(string)
	u.execve = func(p string, _, _ []string) error { *execed = p; return nil }
	u.exit = func(int) {}
	u.readyTimeout = 5 * time.Second
	u.termTimeout = 100 * time.Millisecond
	u.drainTimeout = 5 * time.Second
	u.start()
	return u, execed
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// installScript installs body at path the way an update does, leaving the
// previous file at path+".old" and a pending marker behind.
func installScript(t *testing.T, path, body string) release.Hash {
	t.Helper()
	data := []byte("#!/bin/sh\n" + body + "\n")
	h := release.HashBytes(data)
	if err := (&release.Installer{Path: path}).Install(data, h); err != nil {
		t.Fatal(err)
	}
	return h
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The decision table of docs/DESIGN.md 6.5, without an exec: a start-up
// reverts exactly when a pending marker and a previous binary are both there
// and no handoff launched this process.
func TestSelfCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		pending, old, inherited bool
		wantRevert              bool
		wantMarker              bool
	}{
		{name: "a clean start does nothing"},
		{name: "pending and old on a fresh start revert", pending: true, old: true, wantRevert: true},
		{name: "pending and old under a handoff are the parent's business", pending: true, old: true, inherited: true, wantMarker: true},
		{name: "pending with nothing to revert to drops the marker", pending: true},
		{name: "pending with nothing to revert to under a handoff is left alone", pending: true, inherited: true, wantMarker: true},
		{name: "an old binary without a marker is a completed upgrade", old: true},
		{name: "an old binary without a marker under a handoff", old: true, inherited: true},
		{name: "a handoff with nothing on disk", inherited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "app")
			writeScript(t, path, "exit 0")

			// Built before the markers, so start's own check sees a clean
			// directory and the table below is the only thing under test.
			u, execed := testUpdater(t, path, nil)

			h := release.HashBytes([]byte("release"))
			if tc.old {
				writeScript(t, u.inst.OldPath(), "exit 1")
			}
			if tc.pending {
				if err := os.WriteFile(u.inst.PendingPath(), []byte(h.String()), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := u.selfCheck(tc.inherited); err != nil {
				t.Fatalf("selfCheck: %v", err)
			}

			if got := *execed != ""; got != tc.wantRevert {
				t.Errorf("exec = %v, want %v", got, tc.wantRevert)
			}
			if tc.wantRevert {
				if *execed != path {
					t.Errorf("exec of %q, want %q", *execed, path)
				}
				if got := read(t, path); got != "#!/bin/sh\nexit 1\n" {
					t.Errorf("path holds %q, want the previous binary", got)
				}
				if failed, ok := u.inst.Failed(); !ok || failed != h {
					t.Errorf("failed marker = %v %v, want %v", failed, ok, h)
				}
			}
			if got := exists(u.inst.PendingPath()); got != tc.wantMarker {
				t.Errorf("pending marker present = %v, want %v", got, tc.wantMarker)
			}
		})
	}
}

// Start runs the check before it does anything else, so a release that
// crashed on its first run never gets as far as opening a socket.
func TestStartReverts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	writeScript(t, path, "exit 0")
	h := installScript(t, path, "exit 3")

	u, execed := testUpdater(t, path, nil)
	if *execed != path {
		t.Errorf("Start exec'd %q, want %q", *execed, path)
	}
	if got := read(t, path); got != "#!/bin/sh\nexit 0\n" {
		t.Errorf("path holds %q, want the previous binary", got)
	}
	if failed, ok := u.inst.Failed(); !ok || failed != h {
		t.Errorf("failed marker = %v %v, want %v", failed, ok, h)
	}
	if exists(u.inst.PendingPath()) {
		t.Error("the pending marker survived the revert")
	}
}

func TestListenInheritsByNetworkAndAddress(t *testing.T) {
	t.Parallel()
	handed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer handed.Close()
	f, err := dupListener(handed, "test")
	if err != nil {
		t.Fatal(err)
	}

	u, _ := testUpdater(t, filepath.Join(t.TempDir(), "app"), nil)
	u.inherited = []*inheritedFile{{spec: fdSpec{FD: 3, Network: "tcp", Addr: "127.0.0.1:0"}, f: f}}

	got, err := u.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.Addr().String() != handed.Addr().String() {
		t.Fatalf("Listen = %s, want the inherited socket on %s", got.Addr(), handed.Addr())
	}

	// The same pair a second time, and any other pair, is a fresh socket.
	again, err := u.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Addr().String() == handed.Addr().String() {
		t.Error("Listen handed out the inherited socket twice")
	}
	if _, err := u.Listen("tcp", "127.0.0.1:1"); err == nil {
		t.Error("Listen on a privileged port succeeded; it should not have been inherited")
	}
	if n := len(u.serving); n != 2 {
		t.Errorf("serving %d listeners, want 2", n)
	}
}

func TestReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	writeScript(t, path, "exit 0")

	u, _ := testUpdater(t, path, nil)
	installScript(t, path, "exit 0") // leaves a pending marker, as an install does

	unused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer unused.Close()
	f, err := dupListener(unused, "test")
	if err != nil {
		t.Fatal(err)
	}
	u.inherited = []*inheritedFile{{spec: fdSpec{FD: 3, Network: "tcp", Addr: "nobody asked"}, f: f}}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	u.readyPipe = pw

	u.Ready()
	u.Ready() // the second call must do nothing at all

	if exists(u.inst.PendingPath()) {
		t.Error("Ready left the pending marker behind")
	}
	select {
	case <-u.ready:
	default:
		t.Error("Ready did not release the update loop")
	}
	// One byte, then EOF: the parent's read must not block and must not see
	// a second report.
	b, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 {
		t.Errorf("the parent read %d bytes, want 1", len(b))
	}
	if _, err := f.Stat(); err == nil {
		t.Error("Ready left an unclaimed inherited descriptor open")
	}
}

func TestStopClosesDone(t *testing.T) {
	t.Parallel()
	u, _ := testUpdater(t, filepath.Join(t.TempDir(), "app"), nil)
	select {
	case <-u.Done():
		t.Fatal("Done closed before the process was superseded")
	default:
	}
	u.Stop()
	select {
	case <-u.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not close Done")
	}
}
