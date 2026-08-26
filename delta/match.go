package delta

import (
	"encoding/binary"
	"regexp"
	"sync"

	"binsync/delta/gobin"
	"binsync/delta/x86"
)

// match is the correspondence between the old release's functions and the
// new one's. It is the hinge the whole transform turns on: a stripped Go
// binary still names every function in its pclntab, so the two releases can
// be aligned by name rather than by guessing from bytes.
type match struct {
	NewToOld                                      []int // index into old.Funcs, or -1
	OldToNew                                      []int // index into new.Funcs, or -1
	Exact, Norm, Content, Unmatched, UnmatchedOld int
}

var (
	reFuncN   = regexp.MustCompile(`\.func\d+`)
	reDeferN  = regexp.MustCompile(`\.deferwrap\d+`)
	reGowrapN = regexp.MustCompile(`\.gowrap\d+`)
)

// normName collapses the compiler's closure and wrapper numbering, so that a
// closure renumbered by an edit earlier in its file still pairs with itself.
func normName(s string) string {
	s = reFuncN.ReplaceAllString(s, ".func#")
	s = reDeferN.ReplaceAllString(s, ".deferwrap#")
	return reGowrapN.ReplaceAllString(s, ".gowrap#")
}

// matchFuncs pairs functions by exact name, then by normalised name, then by
// the content hash of their code with PC-relative fields masked -- which
// catches a function that was renamed but not changed.
func matchFuncs(old, new *gobin.Bin) *match {
	m := &match{NewToOld: make([]int, len(new.Funcs)), OldToNew: make([]int, len(old.Funcs))}
	for i := range m.NewToOld {
		m.NewToOld[i] = -1
	}
	for i := range m.OldToNew {
		m.OldToNew[i] = -1
	}
	pair := func(key func(*gobin.Bin, int) string) int {
		byKey := map[string][]int{}
		for i := range old.Funcs {
			if m.OldToNew[i] < 0 {
				k := key(old, i)
				byKey[k] = append(byKey[k], i)
			}
		}
		n := 0
		for j := range new.Funcs {
			if m.NewToOld[j] >= 0 {
				continue
			}
			k := key(new, j)
			if c := byKey[k]; len(c) > 0 {
				byKey[k] = c[1:]
				m.NewToOld[j], m.OldToNew[c[0]] = c[0], j
				n++
			}
		}
		return n
	}
	m.Exact = pair(func(b *gobin.Bin, i int) string { return b.Funcs[i].Name })
	m.Norm = pair(func(b *gobin.Bin, i int) string { return normName(b.Funcs[i].Name) })

	oldHash := contentHashes(old, m.OldToNew)
	newHash := contentHashes(new, m.NewToOld)
	m.Content = pair(func(b *gobin.Bin, i int) string {
		h := oldHash
		if b == new {
			h = newHash
		}
		var k [9]byte
		binary.LittleEndian.PutUint64(k[1:], h[i])
		return string(k[:])
	})
	for j := range m.NewToOld {
		if m.NewToOld[j] < 0 {
			m.Unmatched++
		}
	}
	for i := range m.OldToNew {
		if m.OldToNew[i] < 0 {
			m.UnmatchedOld++
		}
	}
	return m
}

// contentHashes hashes the still-unmatched functions of b, in parallel.
func contentHashes(b *gobin.Bin, paired []int) []uint64 {
	out := make([]uint64, len(b.Funcs))
	var wg sync.WaitGroup
	for w := range predictWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(b.Funcs); i += predictWorkers {
				if paired[i] < 0 {
					out[i] = x86.ContentHash(b.FuncBytes(b.Funcs[i]))
				}
			}
		}(w)
	}
	wg.Wait()
	return out
}
