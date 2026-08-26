package release

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	oldBin = []byte("#!/bin/true\nold release\n")
	newBin = []byte("#!/bin/true\nnew release, a different length\n")
)

// target lays out a temp directory holding oldBin at <dir>/server.
func target(t *testing.T) *Installer {
	t.Helper()
	i := &Installer{Path: filepath.Join(t.TempDir(), "server")}
	if err := os.WriteFile(i.Path, oldBin, 0o755); err != nil {
		t.Fatal(err)
	}
	return i
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantContent(t *testing.T, path string, want []byte) {
	t.Helper()
	if got := read(t, path); !bytes.Equal(got, want) {
		t.Fatalf("%s holds %q, want %q", path, got, want)
	}
}

func wantAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists, want it gone (%v)", path, err)
	}
}

// wantNoTemps checks the install left no debris beside the files it owns.
func wantNoTemps(t *testing.T, i *Installer) {
	t.Helper()
	m, err := filepath.Glob(i.Path + ".tmp.*")
	if err != nil || len(m) != 0 {
		t.Fatalf("temp files left behind: %v (%v)", m, err)
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()
	i := target(t)
	if err := i.Install(newBin, HashBytes(newBin)); err != nil {
		t.Fatal(err)
	}

	wantContent(t, i.Path, newBin)
	wantContent(t, i.OldPath(), oldBin)
	wantNoTemps(t, i)
	if fi, err := os.Stat(i.Path); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v (%v), want 0755", fi.Mode().Perm(), err)
	}

	h, ok, err := i.Pending()
	if err != nil || !ok || h != HashBytes(newBin) {
		t.Fatalf("Pending = %v, %v, %v; want the new hash", h, ok, err)
	}
	wantContent(t, i.PendingPath(), []byte(HashBytes(newBin).String()))

	if err := i.ClearPending(); err != nil {
		t.Fatal(err)
	}
	wantAbsent(t, i.PendingPath())
	if _, ok, err := i.Pending(); ok || err != nil {
		t.Fatalf("Pending after clear = %v, %v", ok, err)
	}
	if err := i.ClearPending(); err != nil { // idempotent
		t.Fatal(err)
	}
}

// The running process must keep executing the inode it started, so .old is a
// hard link to it rather than a copy or a rename.
func TestInstallKeepsRunningInode(t *testing.T) {
	t.Parallel()
	i := target(t)
	before, err := os.Stat(i.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Install(newBin, HashBytes(newBin)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(i.OldPath())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal(".old is not the inode that was at the path")
	}
	if now, err := os.Stat(i.Path); err != nil || os.SameFile(before, now) {
		t.Fatalf("path still holds the old inode (%v)", err)
	}
}

func TestInstallRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, want string
		setup      func(t *testing.T, i *Installer)
	}{
		{name: "symlink", want: "symlink", setup: func(t *testing.T, i *Installer) {
			real := i.Path + ".real"
			if err := os.WriteFile(real, oldBin, 0o755); err != nil {
				t.Fatal(err)
			}
			os.Remove(i.Path)
			if err := os.Symlink(real, i.Path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "world-writable", want: "world-writable", setup: func(t *testing.T, i *Installer) {
			if err := os.Chmod(i.Path, 0o757); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "read-only directory", want: "not writable", setup: func(t *testing.T, i *Installer) {
			if os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			dir := filepath.Dir(i.Path)
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := target(t)
			tc.setup(t, i)

			err := i.Install(newBin, HashBytes(newBin))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Install = %v, want an error mentioning %q", err, tc.want)
			}
			wantContent(t, i.Path, oldBin) // through the symlink, in that case
			wantAbsent(t, i.PendingPath())
			wantAbsent(t, i.OldPath())
			wantNoTemps(t, i)
		})
	}
}

// A wrong hash must be caught while the bytes are still only a temp file.
func TestInstallWrongHashNeverVisible(t *testing.T) {
	t.Parallel()
	i := target(t)
	err := i.Install(newBin, HashBytes([]byte("some other release")))
	if err == nil || !strings.Contains(err.Error(), "hash to") {
		t.Fatalf("Install = %v, want a hash mismatch", err)
	}
	wantContent(t, i.Path, oldBin)
	wantAbsent(t, i.OldPath())
	wantAbsent(t, i.PendingPath())
	wantNoTemps(t, i)
}

func TestRevert(t *testing.T) {
	t.Parallel()
	i := target(t)
	if err := i.Install(newBin, HashBytes(newBin)); err != nil {
		t.Fatal(err)
	}
	if err := i.Revert(); err != nil {
		t.Fatal(err)
	}
	wantContent(t, i.Path, oldBin)
	wantAbsent(t, i.PendingPath())
	wantAbsent(t, i.OldPath())

	// Nothing left to revert: the caller hears about it rather than
	// believing the previous release is back.
	if err := i.Revert(); err == nil {
		t.Fatal("Revert on a clean path = nil, want an error")
	}
}

// A crash between the rename and the unlink leaves .old gone and .pending
// present; resuming the revert must succeed rather than trip over itself.
func TestRevertResumesAfterCrash(t *testing.T) {
	t.Parallel()
	i := target(t)
	if err := i.Install(newBin, HashBytes(newBin)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(i.OldPath(), i.Path); err != nil {
		t.Fatal(err)
	}
	if err := i.Revert(); err != nil {
		t.Fatalf("Revert = %v, want it to finish the job", err)
	}
	wantContent(t, i.Path, oldBin)
	wantAbsent(t, i.PendingPath())
}

// The first install on a host has no previous release to link.
func TestInstallOntoNothing(t *testing.T) {
	t.Parallel()
	i := &Installer{Path: filepath.Join(t.TempDir(), "server")}
	if err := i.Install(newBin, HashBytes(newBin)); err != nil {
		t.Fatal(err)
	}
	wantContent(t, i.Path, newBin)
	wantAbsent(t, i.OldPath())

	// There is no previous binary, but the marker must still be cleared.
	if err := i.Revert(); err != nil {
		t.Fatal(err)
	}
	wantAbsent(t, i.PendingPath())
	wantContent(t, i.Path, newBin)
}

// Drive Install one syscall at a time and stop after each, the way a power
// loss would: the path must always hold one whole binary, the documented
// recovery must produce the old one, and a retry must reach the new one.
func TestInstallCrashAfterEachStep(t *testing.T) {
	t.Parallel()
	steps := []string{"temp file written", "linked .old", "wrote .pending", "renamed", "synced dir"}

	for stop := 1; stop <= len(steps); stop++ {
		t.Run(steps[stop-1], func(t *testing.T) {
			t.Parallel()
			i := target(t)
			want := HashBytes(newBin)

			tmp, err := i.stage(newBin, want)
			if err != nil {
				t.Fatal(err)
			}
			if stop >= 2 {
				if err := i.linkOld(); err != nil {
					t.Fatal(err)
				}
			}
			if stop >= 3 {
				if err := i.markPending(want); err != nil {
					t.Fatal(err)
				}
			}
			if stop >= 4 {
				if err := os.Rename(tmp, i.Path); err != nil {
					t.Fatal(err)
				}
			}
			if stop >= 5 {
				if err := syncDir(filepath.Dir(i.Path)); err != nil {
					t.Fatal(err)
				}
			}

			// The invariant: one whole binary, never a partial one.
			got := read(t, i.Path)
			if !bytes.Equal(got, oldBin) && !bytes.Equal(got, newBin) {
				t.Fatalf("path holds %q, neither release", got)
			}

			// Recovery, docs/DESIGN.md 6.5: a pending marker plus an .old
			// means the upgrade never reported ready.
			h, pending, err := i.Pending()
			if err != nil {
				t.Fatal(err)
			}
			_, oldErr := os.Lstat(i.OldPath())
			if pending && oldErr == nil {
				if h != want {
					t.Fatalf("pending marker = %v, want %v", h, want)
				}
				if err := i.Revert(); err != nil {
					t.Fatal(err)
				}
				wantContent(t, i.Path, oldBin)
				wantAbsent(t, i.PendingPath())
			} else if bytes.Equal(got, newBin) {
				t.Fatal("the new release is visible with no way to revert it")
			}

			// Whatever the crash left behind, a retry converges.
			os.Remove(tmp)
			if err := i.Install(newBin, want); err != nil {
				t.Fatal(err)
			}
			wantContent(t, i.Path, newBin)
			wantContent(t, i.OldPath(), oldBin)
			wantNoTemps(t, i)
		})
	}
}

func TestFailedMarker(t *testing.T) {
	t.Parallel()
	i := target(t)
	if h, ok := i.Failed(); ok {
		t.Fatalf("Failed on a fresh target = %v, true", h)
	}

	bad := HashBytes(newBin)
	if err := i.MarkFailed(bad); err != nil {
		t.Fatal(err)
	}
	if h, ok := i.Failed(); !ok || h != bad {
		t.Fatalf("Failed = %v, %v; want %v, true", h, ok, bad)
	}
	if dir := i.StateDir(); dir != i.Path+".binsync" {
		t.Fatalf("StateDir = %q", dir)
	}

	if err := i.ClearFailed(); err != nil {
		t.Fatal(err)
	}
	if _, ok := i.Failed(); ok {
		t.Fatal("Failed after clear = true")
	}
	if err := i.ClearFailed(); err != nil { // idempotent
		t.Fatal(err)
	}
}

// An unreadable marker must read as "nothing failed": retrying a bad release
// is recoverable, refusing to update because a state file is corrupt is not.
func TestFailedMarkerUnreadable(t *testing.T) {
	t.Parallel()
	i := target(t)
	if err := os.MkdirAll(i.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(i.StateDir(), "failed"), []byte("not a hash"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h, ok := i.Failed(); ok {
		t.Fatalf("Failed = %v, true; want it ignored", h)
	}
}

// A truncated .pending still means "an upgrade was in flight"; only its
// content is unusable.
func TestPendingUnreadable(t *testing.T) {
	t.Parallel()
	i := target(t)
	if err := os.WriteFile(i.PendingPath(), []byte("b3:zz"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, ok, err := i.Pending()
	if err == nil || !ok || !h.IsZero() {
		t.Fatalf("Pending = %v, %v, %v; want zero, true, an error", h, ok, err)
	}
}
