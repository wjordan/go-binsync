package main

import (
	"encoding/binary"
	"hash/fnv"
	"regexp"
	"strings"

	"golang.org/x/arch/x86/x86asm"
)

// Match is a correspondence between old and new functions.
type Match struct {
	NewToOld                                      []int  // index into old.Funcs or -1
	OldToNew                                      []int  // index into new.Funcs or -1
	How                                           []byte // per new func: 'e' exact name, 'n' normalised name, 'c' content, 0 unmatched
	Exact, Norm, Content, Unmatched, UnmatchedOld int
}

var (
	reFuncN    = regexp.MustCompile(`\.func\d+`)
	reDeferN   = regexp.MustCompile(`\.deferwrap\d+`)
	reGowrapN  = regexp.MustCompile(`\.gowrap\d+`)
	reShape    = regexp.MustCompile(`\[go\.shape\.[^\]]*\]`)
	reDictName = regexp.MustCompile(`\.dict\.`)
)

// normName collapses closure / wrapper numbering so that renumbered
// closures can still be paired by position.
func normName(s string) string {
	s = reFuncN.ReplaceAllString(s, ".func#")
	s = reDeferN.ReplaceAllString(s, ".deferwrap#")
	s = reGowrapN.ReplaceAllString(s, ".gowrap#")
	return s
}

// nameCategory classifies a name by the stability issue it may have.
func nameCategories(s string) []string {
	var c []string
	if reFuncN.MatchString(s) {
		c = append(c, "closure(.funcN)")
	}
	if reDeferN.MatchString(s) {
		c = append(c, "deferwrap")
	}
	if reGowrapN.MatchString(s) {
		c = append(c, "gowrap")
	}
	if reShape.MatchString(s) {
		c = append(c, "generic[go.shape]")
	}
	if strings.HasSuffix(s, "-fm") {
		c = append(c, "method-value(-fm)")
	}
	if strings.HasPrefix(s, "type:.eq.") || strings.HasPrefix(s, "type:.hash.") {
		c = append(c, "type:.eq/.hash")
	}
	if strings.Contains(s, ".abi0") || strings.HasSuffix(s, ".abiinternal") {
		c = append(c, "abi-wrapper")
	}
	if strings.HasPrefix(s, "go:") || strings.HasPrefix(s, "_cgo") || strings.HasPrefix(s, "x_cgo") || strings.HasPrefix(s, "_rt0") {
		c = append(c, "linker/cgo/rt0")
	}
	return c
}

// contentHash hashes a function's bytes with every PC-relative field zeroed,
// so that a function whose only change is displacement values hashes equal.
func contentHash(code []byte) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(len(code)))
	h.Write(buf[:])
	for i := 0; i < len(code); {
		inst, err := x86asm.Decode(code[i:], 64)
		if err != nil || inst.Len == 0 {
			h.Write(code[i : i+1])
			i++
			continue
		}
		if inst.PCRel > 0 {
			h.Write(code[i : i+inst.PCRelOff])
			h.Write(make([]byte, inst.PCRel))
			h.Write(code[i+inst.PCRelOff+inst.PCRel : i+inst.Len])
		} else {
			h.Write(code[i : i+inst.Len])
		}
		i += inst.Len
	}
	return h.Sum64()
}

// matchFuncs pairs functions of old and new: by exact name (occurrences
// paired in order), then by normalised name, then by size+content hash.
func matchFuncs(old, new *Bin) *Match {
	m := &Match{NewToOld: make([]int, len(new.Funcs)), OldToNew: make([]int, len(old.Funcs)), How: make([]byte, len(new.Funcs))}
	for i := range m.NewToOld {
		m.NewToOld[i] = -1
	}
	for i := range m.OldToNew {
		m.OldToNew[i] = -1
	}
	pair := func(key func(*Func) string, how byte) int {
		oldBy := map[string][]int{}
		for i, f := range old.Funcs {
			if m.OldToNew[i] < 0 {
				k := key(f)
				oldBy[k] = append(oldBy[k], i)
			}
		}
		n := 0
		for j, f := range new.Funcs {
			if m.NewToOld[j] >= 0 {
				continue
			}
			k := key(f)
			if c := oldBy[k]; len(c) > 0 {
				i := c[0]
				oldBy[k] = c[1:]
				m.NewToOld[j], m.OldToNew[i], m.How[j] = i, j, how
				n++
			}
		}
		return n
	}
	m.Exact = pair(func(f *Func) string { return f.Name }, 'e')
	m.Norm = pair(func(f *Func) string { return normName(f.Name) }, 'n')
	hashes := map[*Func]string{}
	for i, f := range old.Funcs {
		if m.OldToNew[i] < 0 {
			hashes[f] = hashKey(contentHash(old.funcBytes(f)))
		}
	}
	for j, f := range new.Funcs {
		if m.NewToOld[j] < 0 {
			hashes[f] = hashKey(contentHash(new.funcBytes(f)))
		}
	}
	m.Content = pair(func(f *Func) string { return hashes[f] }, 'c')
	for j := range new.Funcs {
		if m.NewToOld[j] < 0 {
			m.Unmatched++
		}
	}
	for i := range old.Funcs {
		if m.OldToNew[i] < 0 {
			m.UnmatchedOld++
		}
	}
	return m
}

func hashKey(h uint64) string {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], h)
	return "#" + string(b[:])
}

// encodeLayout encodes new's function list (name, size) as a delta against
// old's: op 0 = same name as the next expected old index (implicit), op 1 =
// same name as explicit old index (varint), op 2 = new name (len + bytes);
// then zigzag varint of size delta (vs the old function, or vs 0 for new).
func encodeLayout(old, new *Bin, m *Match) []byte {
	var out []byte
	var tmp [binary.MaxVarintLen64]byte
	putU := func(v uint64) { n := binary.PutUvarint(tmp[:], v); out = append(out, tmp[:n]...) }
	putS := func(v int64) { n := binary.PutVarint(tmp[:], v); out = append(out, tmp[:n]...) }
	expect := 0
	for j, f := range new.Funcs {
		i := m.NewToOld[j]
		var oldSize int64
		switch {
		case i < 0:
			out = append(out, 2)
			putU(uint64(len(f.Name)))
			out = append(out, f.Name...)
		case i == expect:
			out = append(out, 0)
			oldSize = int64(old.Funcs[i].Size())
			expect = i + 1
		default:
			out = append(out, 1)
			putU(uint64(i))
			oldSize = int64(old.Funcs[i].Size())
			expect = i + 1
		}
		putS(int64(f.Size()) - oldSize)
	}
	return out
}
