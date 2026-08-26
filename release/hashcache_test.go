package release

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cachePath(i *Installer) string { return filepath.Join(i.StateDir(), "hash") }

// poison rewrites the cached hash, keeping the file identity it was written
// for: a later CachedHash that returns it proves the cache was consulted.
func poison(t *testing.T, i *Installer) Hash {
	t.Helper()
	fields := strings.Fields(string(read(t, cachePath(i))))
	if len(fields) != 5 {
		t.Fatalf("cache line has %d fields: %q", len(fields), fields)
	}
	h := HashBytes([]byte("not the file"))
	line := fmt.Sprintf("%s %s %s %s %s\n", fields[0], fields[1], fields[2], fields[3], h)
	if err := os.WriteFile(cachePath(i), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCachedHash(t *testing.T) {
	t.Parallel()
	i := target(t)

	got, err := CachedHash(i.Path)
	if err != nil {
		t.Fatal(err)
	}
	if want := HashBytes(oldBin); got != want {
		t.Fatalf("CachedHash = %v, want %v", got, want)
	}

	poisoned := poison(t, i)
	if got, err := CachedHash(i.Path); err != nil || got != poisoned {
		t.Fatalf("CachedHash = %v, %v; want the cached %v", got, err, poisoned)
	}
}

func TestCachedHashRehashes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		replace func(t *testing.T, i *Installer)
	}{
		{name: "written in place", replace: func(t *testing.T, i *Installer) {
			if err := os.WriteFile(i.Path, newBin, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "renamed over", replace: func(t *testing.T, i *Installer) {
			// Same size and mtime as the old file: only the inode differs,
			// which is exactly what an install does.
			fi, err := os.Stat(i.Path)
			if err != nil {
				t.Fatal(err)
			}
			tmp := i.Path + ".tmp"
			if err := os.WriteFile(tmp, oldBin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(tmp, time.Time{}, fi.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, i.Path); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := target(t)
			if _, err := CachedHash(i.Path); err != nil {
				t.Fatal(err)
			}
			poison(t, i)
			tc.replace(t, i)

			want, err := HashFile(i.Path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := CachedHash(i.Path)
			if err != nil || got != want {
				t.Fatalf("CachedHash = %v, %v; want the re-hashed %v", got, err, want)
			}
			// The stale entry must have been replaced, not just skipped.
			if got, err := CachedHash(i.Path); err != nil || got != want {
				t.Fatalf("CachedHash = %v, %v; want %v", got, err, want)
			}
		})
	}
}

// A target whose directory it cannot write still updates; it just pays the
// hash every cycle.
func TestCachedHashUncacheable(t *testing.T) {
	t.Parallel()
	i := target(t)
	if err := os.WriteFile(i.StateDir(), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := CachedHash(i.Path)
		if err != nil || got != HashBytes(oldBin) {
			t.Fatalf("CachedHash = %v, %v; want %v", got, err, HashBytes(oldBin))
		}
	}
}

func TestCachedHashMissing(t *testing.T) {
	t.Parallel()
	if _, err := CachedHash(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("CachedHash on a missing file = nil error")
	}
}

// A garbage cache line must cost a re-hash, never a wrong answer.
func TestCachedHashCorruptEntry(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"", "garbage", "1 2 3", "1 2 3 4 nothex", "1 2 3 4 b3:00"} {
		i := target(t)
		if err := os.MkdirAll(i.StateDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath(i), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := CachedHash(i.Path)
		if err != nil || got != HashBytes(oldBin) {
			t.Fatalf("CachedHash with cache %q = %v, %v", line, got, err)
		}
	}
}
