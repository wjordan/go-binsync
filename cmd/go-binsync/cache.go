package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wjordan/go-binsync/release"
)

// The release cache holds the recent binaries this machine published, so that
// the next publish encodes its patch without fetching the previous release
// back out of the store (docs/DESIGN.md 4.4). It is only ever an
// optimisation: a cold cache costs one blob download, and a corrupt entry is
// caught by its own name, which is its hash.
const cacheLimit = 10

type releaseCache struct{ dir string }

// openCache prepares the cache directory. dir empty takes
// $XDG_CACHE_HOME/go-binsync.
func openCache(dir string) (*releaseCache, error) {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("no cache directory: %w (pass --cache DIR)", err)
		}
		dir = filepath.Join(base, "go-binsync")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &releaseCache{dir}, nil
}

func (c *releaseCache) path(h release.Hash) string {
	return filepath.Join(c.dir, hex.EncodeToString(h[:]))
}

// get returns the cached binary, or nil if it is not there. The bytes are
// verified: a file that does not hash to its own name is not the release it
// claims to be, and publishing a patch against it would produce a patch no
// target could apply.
func (c *releaseCache) get(h release.Hash) []byte {
	p := c.path(h)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	if release.HashBytes(b) != h {
		os.Remove(p)
		return nil
	}
	now := time.Now()
	os.Chtimes(p, now, now) // eviction order is use order
	return b
}

func (c *releaseCache) put(h release.Hash, data []byte) error {
	p := c.path(h)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	c.evict()
	return nil
}

// evict keeps the cacheLimit most recently used entries and is best-effort:
// a cache that cannot be trimmed is a disk-space problem, not a publishing
// failure.
func (c *releaseCache) evict() {
	ents, err := os.ReadDir(c.dir)
	if err != nil || len(ents) <= cacheLimit {
		return
	}
	type ent struct {
		name string
		mod  time.Time
	}
	var all []ent
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		all = append(all, ent{e.Name(), fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for _, e := range all[min(cacheLimit, len(all)):] {
		os.Remove(filepath.Join(c.dir, e.name))
	}
}
