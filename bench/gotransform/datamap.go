package main

import (
	"fmt"
	"slices"
	"sort"
	"sync"
)

// DataMap maps offsets in an old byte array to offsets in a new one by
// content: every `block`-aligned window of old is looked up (at any
// alignment) in new, and the candidate whose shift is closest to the
// previous block's shift wins. Unmatched blocks inherit the previous shift.
// This is what a decoder could derive from the data section's own patch
// (copy-block control stream), so it costs no extra transmitted bytes.
type DataMap struct {
	Block     int
	Delta     []int64 // per old block: newOff - oldOff
	Matched   []bool
	Ambiguous []bool // encoder only: resolved in the backward pass, not by content
	OldLen    int
	NewLen    int
	Stats     DataMapStats
}

type DataMapStats struct {
	Blocks, Matched, Ambiguous, Unmatched int
	ShiftChanges                          int
}

// The index over the new section records every window position (sampleBits
// = 0; a value n keeps 1 in 2^n windows by rolling hash, the round-3
// setting was 3) in a flat array of (hash32, pos) pairs bucketed by the top
// 16 hash bits and sorted by (hash, position) within a bucket. An old
// block that fails at the previous shift looks up the windows in
// [o, o+block+lookahead); inside a chain only the positions nearest to
// q+prev are verified against the block's own bytes, so a chain of any
// length costs O(log n) and the match is still exact and at any alignment.
// Windows with >= maxChain occurrences count as ambiguous (resolved in the
// backward pass as before).
const (
	sampleShift = 24
	rollBase    = 0x9E3779B97F4A7C15
	bucketBits  = 16
	mapWorkers  = 24
)

var (
	sampleBits = 0
	lookahead  = 48
	maxChain   = 2048
)

func hashWindow(b []byte) uint64 {
	var h uint64
	for _, c := range b {
		h = h*rollBase + uint64(c) + 1
	}
	return h
}

func selected(h uint64) bool { return (h>>sampleShift)&(1<<uint(sampleBits)-1) == 0 }

type idxEnt struct {
	h uint32 // top 32 bits of the rolling hash
	p uint32
}

type winIndex struct {
	block  int
	ent    []idxEnt
	bucket []uint32 // ent index of the first entry with h>>(32-bucketBits) >= b
}

func rollPow(block int) uint64 {
	pow := uint64(1)
	for i := 0; i < block; i++ {
		pow *= rollBase
	}
	return pow
}

func buildWinIndex(new []byte, block int) *winIndex {
	ix := &winIndex{block: block}
	if len(new) < block {
		return ix
	}
	n := len(new) - block + 1 // window positions
	pow := rollPow(block)
	nw := mapWorkers
	if n < 1<<16 {
		nw = 1
	}
	// Two rolling-hash passes: the first counts the selected windows per
	// bucket (top bucketBits of the hash) and per worker, the second places
	// each entry at its worker's slot in its bucket. No temporary arrays;
	// within a bucket the entries end up in position order (workers cover
	// increasing ranges), then each bucket is sorted by (hash, position).
	const nbk = 1 << bucketBits
	hist := make([]uint32, nw*nbk)
	pass := func(place bool) {
		var wg sync.WaitGroup
		for w := 0; w < nw; w++ {
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
					if selected(h) {
						b := uint32(h >> (64 - bucketBits))
						if place {
							ix.ent[cnt[b]] = idxEnt{uint32(h >> 32), uint32(p)}
						}
						cnt[b]++
					}
					if p+block < len(new) {
						h = h*rollBase + uint64(new[p+block]) + 1 - (uint64(new[p])+1)*pow
					}
				}
			}(w)
		}
		wg.Wait()
	}
	pass(false)
	// exclusive prefix sums in bucket-major, worker-minor order
	ix.bucket = make([]uint32, nbk+1)
	sum := uint32(0)
	for b := 0; b < nbk; b++ {
		ix.bucket[b] = sum
		for w := 0; w < nw; w++ {
			c := hist[w*nbk+b]
			hist[w*nbk+b] = sum
			sum += c
		}
	}
	ix.bucket[nbk] = sum
	ix.ent = make([]idxEnt, sum)
	pass(true)
	// sort each bucket by (h, p) in parallel
	var wg sync.WaitGroup
	for w := 0; w < nw; w++ {
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

// lookup returns the positions recorded for hash h, in increasing order.
func (ix *winIndex) lookup(h uint64) []idxEnt {
	if len(ix.ent) == 0 {
		return nil
	}
	h32 := uint32(h >> 32)
	lo, hi := int(ix.bucket[h32>>(32-bucketBits)]), int(ix.bucket[h32>>(32-bucketBits)+1])
	ents := ix.ent[lo:hi]
	i := sort.Search(len(ents), func(i int) bool { return ents[i].h >= h32 })
	j := i + sort.Search(len(ents)-i, func(k int) bool { return ents[i+k].h > h32 })
	return ents[i:j]
}

// buildDataMap without alignment hints.
func buildDataMap(old, new []byte, block int) *DataMap {
	return buildDataMapAligned(old, new, block, nil)
}

// dataTol is the fuzzy tolerance used for the data-section maps (bytes of a
// block allowed to differ when testing the previous/next shift).
var dataTol = 0

// buildDataMapAligned: align[i] (if non-nil) is the minimum alignment the
// shift of old block i must respect (8 for blocks holding absolute pointers,
// since such symbols are pointer-aligned and cannot move by a non-multiple).
func buildDataMapAligned(old, new []byte, block int, align []int64) *DataMap {
	return buildDataMapFuzzy(old, new, block, align, dataTol)
}

// buildDataMapFuzzy additionally accepts a candidate shift (prev / next / 0)
// when at most tol bytes of the block differ, so that blocks holding a few
// changed embedded offsets (e.g. nameOff fields in inline trees) still map.
//
// The forward pass is sequential in nature (each block starts from the
// previous block's shift), but its only state is that shift, which equals
// Delta[i-1] after every block. So it runs in parallel shards seeded with
// shift 0, and a fix-up pass re-runs each shard's prefix from the true
// incoming shift until the recomputed Delta agrees with the shard's; the
// result is identical to the sequential algorithm.
func buildDataMapFuzzy(old, new []byte, block int, align []int64, tol int) *DataMap {
	m := &DataMap{Block: block, OldLen: len(old), NewLen: len(new)}
	nb := (len(old) + block - 1) / block
	m.Delta = make([]int64, nb)
	m.Matched = make([]bool, nb)
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
		for k := 0; k < block; k++ {
			if old[o+k] != new[int(p)+k] {
				bad++
				if bad > tol {
					return false
				}
			}
		}
		return true
	}
	ambiguous := make([]bool, nb)
	m.Ambiguous = ambiguous
	// step computes block i from the incoming shift prev.
	step := func(i int, prev int64) (delta int64, matched, ambig bool) {
		o := i * block
		if o+block > len(old) {
			return prev, false, false
		}
		// cheap exact test at the previous shift first
		if matchAt(o, prev) {
			return prev, true, false
		}
		// candidate shifts from the indexed windows near this block: per
		// window only the positions nearest to q+prev; a window whose content
		// is too common (>= maxChain positions) is skipped, and the block is
		// ambiguous only if every window was
		best := int64(0)
		bestDist := int64(-1)
		overflow := false
		qEnd := o + block + lookahead
		if qEnd > len(old)-block {
			qEnd = len(old) - block
		}
		h := hashWindow(old[o : o+block])
		for q := o; q <= qEnd; q++ {
			if selected(h) {
				ents := ix.lookup(h)
				if len(ents) >= maxChain {
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
						dist := d - prev
						if dist < 0 {
							dist = -dist
						}
						if bestDist < 0 || dist < bestDist {
							best, bestDist = d, dist
						}
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
		// a shift change must be confirmed by the following block too,
		// otherwise a changed block that happens to match elsewhere
		// (duplicated content) would drag its neighbours along.
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
	for w := 0; w < nw; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var prev int64
			for i := nb * w / nw; i < nb*(w+1)/nw; i++ {
				m.Delta[i], m.Matched[i], ambiguous[i] = step(i, prev)
				prev = m.Delta[i]
			}
		}(w)
	}
	wg.Wait()
	// fix-up: re-run each shard's prefix from the true incoming shift
	for w := 1; w < nw; w++ {
		lo, hi := nb*w/nw, nb*(w+1)/nw
		if lo == 0 || lo >= hi {
			continue
		}
		prev := m.Delta[lo-1]
		for i := lo; i < hi; i++ {
			d, mt, am := step(i, prev)
			if d == m.Delta[i] && mt == m.Matched[i] && am == ambiguous[i] {
				break
			}
			m.Delta[i], m.Matched[i], ambiguous[i] = d, mt, am
			prev = d
		}
	}
	// backward pass: ambiguous blocks try the next resolved block's shift, then 0
	nextDelta := int64(0)
	haveNext := false
	for i := nb - 1; i >= 0; i-- {
		if !ambiguous[i] {
			if m.Matched[i] {
				nextDelta, haveNext = m.Delta[i], true
			}
			continue
		}
		o := i * block
		switch {
		case matchAt(o, m.Delta[i]):
			// prev shift works
		case haveNext && matchAt(o, nextDelta):
			m.Delta[i] = nextDelta
		case matchAt(o, 0):
			m.Delta[i] = 0
		default:
			// keep prev but respect alignment
			a := alignOf(i)
			m.Delta[i] = m.Delta[i] / a * a
		}
	}
	var prev int64
	for i := 0; i < nb; i++ {
		switch {
		case m.Matched[i]:
			m.Stats.Matched++
			if m.Delta[i] != prev {
				m.Stats.ShiftChanges++
			}
		case ambiguous[i]:
			m.Stats.Ambiguous++
		default:
			m.Stats.Unmatched++
		}
		prev = m.Delta[i]
	}
	return m
}

// reliable reports whether the new position of old block i is trustworthy:
// the block matched by content, or it sits in a stable run (the nearest
// matched blocks on both sides carry the same shift as it does).
func (m *DataMap) reliable(i int) bool {
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

// Map returns the new offset for an old offset.
func (m *DataMap) Map(off uint64) uint64 {
	if len(m.Delta) == 0 {
		return off
	}
	i := int(off) / m.Block
	if i >= len(m.Delta) {
		i = len(m.Delta) - 1
	}
	return uint64(int64(off) + m.Delta[i])
}

func (m *DataMap) String() string {
	return fmt.Sprintf("blocks=%d matched=%d ambiguous=%d unmatched=%d shift-changes=%d", m.Stats.Blocks, m.Stats.Matched, m.Stats.Ambiguous, m.Stats.Unmatched, m.Stats.ShiftChanges)
}

// maskPointers returns a copy of d where every 8-byte-aligned little-endian
// qword whose value lies in [lo, hi) is zeroed, so that absolute pointers
// (which move when their targets move) do not defeat content matching.
func maskPointers(d []byte, lo, hi uint64) ([]byte, int, []bool) {
	out := make([]byte, len(d))
	copy(out, d)
	n := 0
	isPtr := make([]bool, len(d)/8+1)
	for i := 0; i+8 <= len(out); i += 8 {
		v := uint64(out[i]) | uint64(out[i+1])<<8 | uint64(out[i+2])<<16 | uint64(out[i+3])<<24 |
			uint64(out[i+4])<<32 | uint64(out[i+5])<<40 | uint64(out[i+6])<<48 | uint64(out[i+7])<<56
		if v >= lo && v < hi {
			for k := 0; k < 8; k++ {
				out[i+k] = 0
			}
			isPtr[i/8] = true
			n++
		}
	}
	return out, n, isPtr
}

// imageRange returns [lowest, highest) virtual address of the loaded image.
func imageRange(b *Bin) (uint64, uint64) {
	lo, hi := ^uint64(0), uint64(0)
	for _, s := range b.Order {
		if s.Addr == 0 {
			continue
		}
		if s.Addr < lo {
			lo = s.Addr
		}
		if s.Addr+s.Size > hi {
			hi = s.Addr + s.Size
		}
	}
	return lo, hi
}

// buildDataMapMasked is buildDataMap over pointer-masked copies.
func buildDataMapMasked(old, new *Bin, name string, block int) (*DataMap, int, int) {
	os, ns := old.Sects[name], new.Sects[name]
	olo, ohi := imageRange(old)
	nlo, nhi := imageRange(new)
	om, on, isPtr := maskPointers(os.Data, olo, ohi)
	nm, nn, _ := maskPointers(ns.Data, nlo, nhi)
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
	return buildDataMapAligned(om, nm, block, align), on, nn
}
