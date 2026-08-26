package selfupdate

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A handoff whose new process never reports ready leaves the previous binary
// at the path, the release recorded as failed, and this process serving
// (README guarantee 7).
func TestHandoffRollsBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, want string
		readyTimeout     time.Duration
	}{
		{name: "the new process exits", body: "exit 3", want: "the new process failed"},
		{name: "the new process never reports ready", body: "exec sleep 30", want: "did not report ready", readyTimeout: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "app")
			writeScript(t, path, "exit 0") // the release in service

			u, _ := testUpdater(t, path, nil)
			if tc.readyTimeout != 0 {
				u.readyTimeout = tc.readyTimeout
			}
			ln, err := u.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go accept(ln)

			h := installScript(t, path, tc.body)
			err = u.restart(h)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("restart = %v, want an error about %q", err, tc.want)
			}

			if got := read(t, path); got != "#!/bin/sh\nexit 0\n" {
				t.Errorf("path holds %q, want the previous binary back", got)
			}
			if exists(u.inst.PendingPath()) {
				t.Error("the pending marker survived the rollback")
			}
			if failed, ok := u.inst.Failed(); !ok || failed != h {
				t.Errorf("failed marker = %v %v, want %v", failed, ok, h)
			}
			select {
			case <-u.Done():
				t.Error("Done closed although the upgrade was rolled back")
			default:
			}
			// Still serving on the socket the child was offered.
			c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
			if err != nil {
				t.Fatalf("dialling after the rollback: %v", err)
			}
			c.Close()
		})
	}
}

func accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

// The file a handoff execs was written moments ago, and Linux refuses to exec
// a file any process still holds open for writing.
func TestStartExecRetriesWhileTheFileIsBusy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app")
	writeScript(t, path, "exit 0")
	busy, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	attempts := 0
	cmd, err := startExec(func() *exec.Cmd {
		attempts++
		if attempts == 3 {
			busy.Close()
		}
		return exec.Command(path)
	})
	if err != nil {
		t.Fatalf("startExec after %d attempts: %v", attempts, err)
	}
	if attempts != 3 {
		t.Errorf("started on attempt %d, want the third", attempts)
	}
	cmd.Wait()
}
