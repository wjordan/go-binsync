package release

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// access(2) modes; the standard syscall package does not name them on Linux.
const wOK, xOK = 0x2, 0x1

// Installer replaces the binary at Path atomically and can put the previous
// one back (docs/DESIGN.md 6.2). It touches only Path, Path+".old",
// Path+".pending", Path+".binsync/" and its own temp files, all of which
// live in Path's directory.
type Installer struct{ Path string }

// OldPath is the hard link to the binary Install replaced.
func (i *Installer) OldPath() string { return i.Path + ".old" }

// PendingPath holds the hash of an install that has not been confirmed
// healthy yet; its presence at start-up means the last upgrade never
// reported ready (docs/DESIGN.md 6.5).
func (i *Installer) PendingPath() string { return i.Path + ".pending" }

// StateDir is the directory for state that outlives one update: the hash
// cache and the failed marker.
func (i *Installer) StateDir() string { return i.Path + ".binsync" }

func (i *Installer) failedPath() string { return filepath.Join(i.StateDir(), "failed") }

// Install writes data as the binary at Path, verifying it hashes to want
// before it can become visible. The steps run in the order docs/DESIGN.md
// 6.2 fixes and each one is idempotent, so a crash at any instant leaves
// either the whole previous binary or the whole new one at Path.
func (i *Installer) Install(data []byte, want Hash) error {
	tmp, err := i.stage(data, want)
	if err != nil {
		return err
	}

	if err := i.linkOld(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := i.markPending(want); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, i.Path); err != nil {
		// Nothing became visible: undo the marker so the next start-up does
		// not revert an install that never happened.
		os.Remove(tmp)
		os.Remove(i.PendingPath())
		return fmt.Errorf("release: install %s: %w", i.Path, err)
	}
	return syncDir(filepath.Dir(i.Path))
}

// stage does everything that cannot damage the running service: the local
// safety checks, the temp file, and the hash check.
func (i *Installer) stage(data []byte, want Hash) (string, error) {
	dir := filepath.Dir(i.Path)
	if err := i.check(dir); err != nil {
		return "", err
	}

	f, err := os.CreateTemp(dir, filepath.Base(i.Path)+".tmp.*")
	if err != nil {
		return "", fmt.Errorf("release: install %s: %w", i.Path, err)
	}
	tmp := f.Name()
	err = writeExec(f, data)
	if err == nil {
		if got := HashBytes(data); got != want {
			err = fmt.Errorf("release: install %s: bytes hash to %s, want %s", i.Path, got, want)
		}
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// check refuses the two local situations README 8 calls out, plus a
// directory the service user cannot write, which would fail later anyway
// but with a worse message.
func (i *Installer) check(dir string) error {
	fi, err := os.Lstat(i.Path)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("release: install %s: %w", i.Path, err)
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("release: install %s: path is a symlink", i.Path)
	case err == nil && fi.Mode().Perm()&0o002 != 0:
		return fmt.Errorf("release: install %s: mode %04o is world-writable", i.Path, fi.Mode().Perm())
	}
	if err := syscall.Access(dir, wOK|xOK); err != nil {
		return fmt.Errorf("release: install %s: directory %s is not writable: %w", i.Path, dir, err)
	}
	return nil
}

// linkOld points OldPath at the inode Path currently holds. A hard link
// rather than a rename, so the name Path never disappears and the running
// process keeps executing that inode.
func (i *Installer) linkOld() error {
	if err := os.Remove(i.OldPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release: install %s: %w", i.Path, err)
	}
	// A missing Path is the first install on this host: there is no previous
	// release to keep, and Revert correctly finds nothing.
	if err := os.Link(i.Path, i.OldPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release: install %s: %w", i.Path, err)
	}
	return nil
}

func (i *Installer) markPending(want Hash) error {
	if err := writeSync(i.PendingPath(), []byte(want.String()), 0o644); err != nil {
		return fmt.Errorf("release: install %s: %w", i.Path, err)
	}
	return nil
}

// Revert puts the previous binary back and clears the pending marker. It is
// idempotent: repeating it, or resuming it after a crash, is a no-op. It
// reports an error only when there is neither a previous binary nor a marker,
// which means there was nothing to revert.
func (i *Installer) Revert() error {
	err := os.Rename(i.OldPath(), i.Path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(i.PendingPath()); serr != nil {
			return fmt.Errorf("release: revert %s: no %s to revert to", i.Path, i.OldPath())
		}
		err = nil // a revert that already got as far as the rename
	}
	if err != nil {
		return fmt.Errorf("release: revert %s: %w", i.Path, err)
	}
	if err := os.Remove(i.PendingPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release: revert %s: %w", i.Path, err)
	}
	return syncDir(filepath.Dir(i.Path))
}

// Pending reports the hash an unconfirmed install wrote. The bool reports
// whether the marker file exists, which is what the start-up check branches
// on, so it can be true alongside an error about unreadable content.
func (i *Installer) Pending() (Hash, bool, error) {
	b, err := os.ReadFile(i.PendingPath())
	if errors.Is(err, fs.ErrNotExist) {
		return Hash{}, false, nil
	}
	if err != nil {
		return Hash{}, false, fmt.Errorf("release: pending marker of %s: %w", i.Path, err)
	}
	h, err := ParseHash(strings.TrimSpace(string(b)))
	if err != nil {
		return Hash{}, true, fmt.Errorf("release: pending marker of %s: %w", i.Path, err)
	}
	return h, true, nil
}

// ClearPending records that the installed release is serving and healthy.
func (i *Installer) ClearPending() error {
	if err := os.Remove(i.PendingPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release: clear pending of %s: %w", i.Path, err)
	}
	return syncDir(filepath.Dir(i.Path))
}

// MarkFailed records a release that was installed and reverted. The caller
// skips it until the pointer names a different head, so a broken release
// cannot crash-loop the target (README guarantee 7).
func (i *Installer) MarkFailed(h Hash) error {
	if err := os.MkdirAll(i.StateDir(), 0o755); err != nil {
		return fmt.Errorf("release: mark %s failed: %w", h, err)
	}
	if err := writeSync(i.failedPath(), []byte(h.String()), 0o644); err != nil {
		return fmt.Errorf("release: mark %s failed: %w", h, err)
	}
	return nil
}

// Failed returns the release last recorded by MarkFailed. A marker that
// cannot be read or does not hold a hash reads as "none": the cost is
// retrying a bad release, which is safer than refusing to update at all.
func (i *Installer) Failed() (Hash, bool) {
	b, err := os.ReadFile(i.failedPath())
	if err != nil {
		return Hash{}, false
	}
	h, err := ParseHash(strings.TrimSpace(string(b)))
	if err != nil || h.IsZero() {
		return Hash{}, false
	}
	return h, true
}

// ClearFailed forgets the failed marker; the caller does this when the
// pointer moves to a different head.
func (i *Installer) ClearFailed() error {
	if err := os.Remove(i.failedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release: clear failed marker of %s: %w", i.Path, err)
	}
	return nil
}

// writeExec fills an open temp file with an executable and closes it.
func writeExec(f *os.File, data []byte) error {
	err := func() error {
		if _, err := f.Write(data); err != nil {
			return err
		}
		// Chmod on the descriptor, not the name: the mode is 0755 whatever
		// the umask, and no other file can be caught by it.
		if err := f.Chmod(0o755); err != nil {
			return err
		}
		return f.Sync()
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("release: write %s: %w", f.Name(), err)
	}
	return nil
}

func writeSync(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncDir makes a rename or unlink in dir survive a power loss.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("release: sync %s: %w", dir, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("release: sync %s: %w", dir, err)
	}
	return nil
}
