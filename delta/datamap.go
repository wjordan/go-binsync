package delta

import (
	"slices"
	"sort"
	"sync"

	"github.com/wjordan/go-binsync/delta/gobin"
)

// A dataMap says where each block of an old data section ended up in the new
// one. It is how the codec follows .rodata, .go.type, .go.func, .noptrdata
// and .data across a release: the linker sorts those sections by size and by
// first use, so an added string or a new type descriptor slides everything
// after it by one constant, and a piecewise-constant shift describes the
// whole section in a few hundred bytes.
//
// The map is derived by content -- every block of the old section is looked
// up in the new one at any alignment -- and then transmitted run-length
// encoded, because the decoder has no new section to look anything up in.
type dataMap struct {
	Block   int
	Delta   []int64 // per old block: newOff - oldOff
	Matched []bool  // matched by content rather than inherited
	// Ambiguous marks blocks whose content occurs too often to place; they
	// are resolved in the backward pass. Encoder side only.
	Ambiguous []bool
	OldLen    int
	Stats     dataMapStats
}

type dataMapStats struct{ Blocks, Matched, Ambiguous, Unmatched, ShiftChanges int }

// Sections predicted through a content map, and sections that are merely
// copied with their absolute pointers re-targeted.
var (
	dataMapSects    = []string{".rodata", ".go.type", ".go.func", ".noptrdata", ".data"}
	ptrRewriteSects = []string{".go.module", ".dynamic", ".got", ".got.plt", ".rela", ".rela.plt", ".go.fipsinfo", ".dynsym"}
)

// The window index over the new section records every window position in a
// flat array of (hash, position) pairs, bucketed by the top bits of the hash
// and sorted by (hash, position) within a bucket. An old block that does not
// match at the previous shift looks up the windows in [o, o+block+lookahead);
// inside a bucket only the positions nearest the expected one are verified
// against the block's own bytes, so a common window costs O(log n) and the
// match is still exact and at any alignment.
const (
	rollBase   = 0x9E3779B97F4A7C15
	bucketBits = 16
	// mapWorkers must be a constant, not a core count: the decoder builds
	// some of these maps too, and encoder and decoder have to agree on the
	// prediction byte for byte.
	mapWorkers = 24
	lookahead  = 48
	// maxOccur is when a window is too common to be worth verifying; the
	// block is then resolved by its neighbours instead.
	maxOccur = 2048
)

func hashWindow(b []byte) uint64 {
	var h uint64
	for _, c := range b {
		h = h*rollBase + uint64(c) + 1
	}
	return h
}

func rollPow(block int) uint64 {
	pow := uint64(1)
	for range block {
		pow *= rollBase
	}
	return pow
}

type idxEnt struct{ h, p uint32 }

type winIndex struct {
	ent    []idxEnt
	bucket []uint32
}

func buildWinIndex(new []byte, block int) *winIndex {
	ix := &winIndex{}
	const nbk = 1 << bucketBits
	ix.bucket = make([]uint32, nbk+1)
	if len(new) < block {
		return ix
	}
	n := len(new) - block + 1
	pow := rollPow(block)
	nw := mapWorkers
	if n < 1<<16 {
		nw = 1
	}
	// Two rolling-hash passes: count per (worker, bucket), then place. No
	// temporary arrays, and within a bucket the entries come out in position
	// order because workers cover increasing ranges.
	hist := make([]uint32, nw*nbk)
	pass := func(place bool) {
		var wg sync.WaitGroup
		for w := range nw {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				lo, hi := n*w/nw, n*(w+1)/nw
				if lo >= hi {
					return
				}
				cnt := hist[w*nbk : (w+1)*nbk]
				h := hashWindow(new[lo : lo+block])
				for p := lo; p < hi; p++ {
					b := uint32(h >> (64 - bucketBits))
					if place {
						ix.ent[cnt[b]] = idxEnt{uint32(h >> 32), uint32(p)}
					}
					cnt[b]++
					if p+block < len(new) {
						h = h*rollBase + uint64(new[p+block]) + 1 - (uint64(new[p])+1)*pow
					}
				}
			}(w)
		}
		wg.Wait()
	}
	pass(false)
	sum := uint32(0)
	for b := range nbk {
		ix.bucket[b] = sum
		for w := range nw {
			c := hist[w*nbk+b]
			hist[w*nbk+b] = sum
			sum += c
		}
	}
	ix.bucket[nbk] = sum
	ix.ent = make([]idxEnt, sum)
	pass(true)
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for b := w; b < nbk; b += nw {
				e := ix.ent[ix.bucket[b]:ix.bucket[b+1]]
				if len(e) > 1 {
					slices.SortFunc(e, func(a, b idxEnt) int {
						if a.h != b.h {
							if a.h < b.h {
								return -1
							}
							return 1
						}
						return int(a.p) - int(b.p)
					})
				}
			}
		}(w)
	}
	wg.Wait()
	return ix
}

func (ix *winIndex) lookup(h uint64) []idxEnt {
	if len(ix.ent) == 0 {
		return nil
	}
	h32 := uint32(h >> 32)
	b := h32 >> (32 - bucketBits)
	ents := ix.ent[ix.bucket[b]:ix.bucket[b+1]]
	i := sort.Search(len(ents), func(i int) bool { return ents[i].h >= h32 })
	j := i + sort.Search(len(ents)-i, func(k int) bool { return ents[i+k].h > h32 })
	return ents[i:j]
}

// buildDataMap maps old onto new in blocks. align[i], when non-nil, is the
// minimum alignment the shift of block i may have: a block holding absolute
// pointers cannot have moved by a non-multiple of the pointer size, and
// saying so keeps a repetitive block from matching at a nonsense offset.
// tol allows that many bytes of a block to differ, so a block whose only
// change is an embedded offset still places.
func buildDataMap(old, new []byte, block int, align []int64, tol int) *dataMap {
	m := &dataMap{Block: block, OldLen: len(old)}
	nb := (len(old) + block - 1) / block
	m.Delta = make([]int64, nb)
	m.Matched = make([]bool, nb)
	m.Ambiguous = make([]bool, nb)
	m.Stats.Blocks = nb
	if len(new) < block || len(old) < block {
		m.Stats.Unmatched = nb
		return m
	}
	ix := buildWinIndex(new, block)
	pow := rollPow(block)
	alignOf := func(i int) int64 {
		if align == nil {
			return 1
		}
		return align[i]
	}
	matchAt := func(o int, d int64) bool {
		if d%alignOf(o/block) != 0 {
			return false
		}
		p := int64(o) + d
		if p < 0 || p+int64(block) > int64(len(new)) {
			return false
		}
		if tol == 0 {
			return string(old[o:o+block]) == string(new[p:p+int64(block)])
		}
		bad := 0
		for k := range block {
			if old[o+k] != new[int(p)+k] {
				if bad++; bad > tol {
					return false
				}
			}
		}
		return true
	}
	// step computes block i from the incoming shift prev. Its only state is
	// that shift, which is why the pass below can be sharded.
	step := func(i int, prev int64) (delta int64, matched, ambig bool) {
		o := i * block
		if o+block > len(old) {
			return prev, false, false
		}
		if matchAt(o, prev) {
			return prev, true, false
		}
		best, bestDist := int64(0), int64(-1)
		overflow := false
		qEnd := min(o+block+lookahead, len(old)-block)
		h := hashWindow(old[o : o+block])
		for q := o; q <= qEnd; q++ {
			ents := ix.lookup(h)
			if len(ents) >= maxOccur {
				overflow = true
			} else if len(ents) > 0 {
				t := int64(q) + prev
				k := sort.Search(len(ents), func(k int) bool { return int64(ents[k].p) >= t })
				for _, c := range [2]int{k - 1, k} {
					if c < 0 || c >= len(ents) {
						continue
					}
					d := int64(ents[c].p) - int64(q)
					if d == prev || !matchAt(o, d) {
						continue
					}
					dist := max(d-prev, prev-d)
					if bestDist < 0 || dist < bestDist {
						best, bestDist = d, dist
					}
				}
			}
			if q+block < len(old) {
				h = h*rollBase + uint64(old[q+block]) + 1 - (uint64(old[q])+1)*pow
			}
		}
		if bestDist < 0 {
			return prev, false, overflow
		}
		// a shift change has to be confirmed by the next block too, or a
		// changed block that happens to occur elsewhere drags its
		// neighbours to a wrong offset
		if o+2*block <= len(old) && !matchAt(o+block, best) && matchAt(o+block, prev) {
			return prev, false, false
		}
		return best, true, false
	}
	nw := mapWorkers
	if nb < 4096 {
		nw = 1
	}
	var wg sync.WaitGroup
	for w := range nw {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var prev int64
			for i := nb * w / nw; i < nb*(w+1)/nw; i++ {
				m.Delta[i], m.Matched[i], m.Ambiguous[i] = step(i, prev)
				prev = m.Delta[i]
			}
		}(w)
	}
	wg.Wait()
	// Each shard started from shift 0; re-run its prefix from the true
	// incoming shift until the recomputed value agrees, which makes the
	// result identical to the sequential pass.
	for w := 1; w < nw; w++ {
		lo, hi := nb*w/nw, nb*(w+1)/nw
		if lo == 0 || lo >= hi {
			continue
		}
		prev := m.Delta[lo-1]
		for i := lo; i < hi; i++ {
			d, mt, am := step(i, prev)
			if d == m.Delta[i] && mt == m.Matched[i] && am == m.Ambiguous[i] {
				break
			}
			m.Delta[i], m.Matched[i], m.Ambiguous[i] = d, mt, am
			prev = d
		}
	}
	// backward pass: an ambiguous block takes the next resolved shift, then 0
	nextDelta, haveNext := int64(0), false
	for i := nb - 1; i >= 0; i-- {
		if !m.Ambiguous[i] {
			if m.Matched[i] {
				nextDelta, haveNext = m.Delta[i], true
			}
			continue
		}
		o := i * block
		switch {
		case matchAt(o, m.Delta[i]):
		case haveNext && matchAt(o, nextDelta):
			m.Delta[i] = nextDelta
		case matchAt(o, 0):
			m.Delta[i] = 0
		default:
			a := alignOf(i)
			m.Delta[i] = m.Delta[i] / a * a
		}
	}
	var prev int64
	for i := range nb {
		switch {
		case m.Matched[i]:
			m.Stats.Matched++
			if m.Delta[i] != prev {
				m.Stats.ShiftChanges++
			}
		case m.Ambiguous[i]:
			m.Stats.Ambiguous++
		default:
			m.Stats.Unmatched++
		}
		prev = m.Delta[i]
	}
	return m
}

// reliable reports whether block i's new position can be trusted: it matched
// by content, or it sits in a run whose nearest matched neighbours on both
// sides carry the same shift.
func (m *dataMap) reliable(i int) bool {
	if i < 0 || i >= len(m.Delta) {
		return false
	}
	if m.Matched[i] {
		return true
	}
	d := m.Delta[i]
	lo := i - 1
	for lo >= 0 && !m.Matched[lo] {
		lo--
	}
	hi := i + 1
	for hi < len(m.Delta) && !m.Matched[hi] {
		hi++
	}
	return lo >= 0 && hi < len(m.Delta) && m.Delta[lo] == d && m.Delta[hi] == d
}

// Map returns the new offset of an old one.
func (m *dataMap) Map(off uint64) uint64 {
	if m == nil || len(m.Delta) == 0 {
		return off
	}
	i := min(int(off)/m.Block, len(m.Delta)-1)
	return uint64(int64(off) + m.Delta[i])
}

// maskPointers zeroes every 8-byte-aligned qword that looks like an address
// in the image, so that pointers -- which move when their targets move --
// do not stop a block from matching by content. It also reports which
// qwords those were, which is what constrains a block's alignment.
func maskPointers(d []byte, lo, hi uint64) (masked []byte, isPtr []bool) {
	masked = make([]byte, len(d))
	copy(masked, d)
	isPtr = make([]bool, len(d)/8+1)
	for i := 0; i+8 <= len(masked); i += 8 {
		v := uint64(masked[i]) | uint64(masked[i+1])<<8 | uint64(masked[i+2])<<16 | uint64(masked[i+3])<<24 |
			uint64(masked[i+4])<<32 | uint64(masked[i+5])<<40 | uint64(masked[i+6])<<48 | uint64(masked[i+7])<<56
		if v >= lo && v < hi {
			clear(masked[i : i+8])
			isPtr[i/8] = true
		}
	}
	return masked, isPtr
}

// buildSectionMap is buildDataMap over pointer-masked copies of one section.
func buildSectionMap(old, new *gobin.Bin, name string, block int) *dataMap {
	os, ns := old.Sects[name], new.Sects[name]
	olo, ohi := old.ImageRange()
	nlo, nhi := new.ImageRange()
	om, isPtr := maskPointers(os.Data, olo, ohi)
	nm, _ := maskPointers(ns.Data, nlo, nhi)
	nb := (len(om) + block - 1) / block
	align := make([]int64, nb)
	for i := range align {
		align[i] = 1
		for q := i * block / 8; q < (i*block+block+7)/8 && q < len(isPtr); q++ {
			if isPtr[q] {
				align[i] = 8
			}
		}
	}
	return buildDataMap(om, nm, block, align, 0)
}
