package release

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// fileID is what a cached hash was computed for. Any change to it means the
// file is a different one: a rename over the path gives a new inode, and an
// in-place write moves size or mtime.
type fileID struct {
	dev, ino      uint64
	size, mtimeNS int64
}

func statID(fi fs.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{uint64(st.Dev), uint64(st.Ino), fi.Size(), fi.ModTime().UnixNano()}, true
}

// CachedHash returns the BLAKE3 of the file at path, hashing it only when
// the cache in <path>.go-binsync/hash was written for a different file. A
// target must know its own release before every update (README guarantee 4),
// and re-reading a 100 MB binary every poll is the one cost that would make
// a 5 s poll interval expensive.
func CachedHash(path string) (Hash, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Hash{}, fmt.Errorf("release: hash %s: %w", path, err)
	}
	id, idOK := statID(fi)
	cache := filepath.Join((&Installer{Path: path}).StateDir(), "hash")
	if idOK {
		if h, ok := readHashCache(cache, id); ok {
			return h, nil
		}
	}

	h, err := HashFile(path)
	if err != nil {
		return Hash{}, fmt.Errorf("release: hash %s: %w", path, err)
	}

	// Only cache what the whole read saw: if the file was replaced while it
	// was being hashed, h belongs to neither id and caching it would pin a
	// wrong answer for as long as the file sits still.
	if after, err := os.Stat(path); err == nil && idOK {
		if id2, ok := statID(after); ok && id2 == id {
			writeHashCache(cache, id, h)
		}
	}
	return h, nil
}

func readHashCache(path string, want fileID) (Hash, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Hash{}, false
	}
	var got fileID
	var hs string
	if n, err := fmt.Sscanf(string(b), "%d %d %d %d %s", &got.dev, &got.ino, &got.size, &got.mtimeNS, &hs); n != 5 || err != nil {
		return Hash{}, false
	}
	if got != want {
		return Hash{}, false
	}
	h, err := ParseHash(hs)
	if err != nil || h.IsZero() {
		return Hash{}, false
	}
	return h, true
}

// writeHashCache is best effort: a read-only directory costs a re-hash every
// cycle, it must never cost an update.
func writeHashCache(path string, id fileID, h Hash) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	line := fmt.Sprintf("%d %d %d %d %s\n", id.dev, id.ino, id.size, id.mtimeNS, h)
	os.WriteFile(path, []byte(line), 0o644)
}
