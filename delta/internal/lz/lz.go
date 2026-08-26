// Package lz is the one exact-match engine in the codec: an index over a
// source buffer and a (literal, copy, seek) op stream.
//
// It is deliberately not bsdiff. bsdiff's approximate matching earns its
// suffix array when the two files are shifted machine code, which is exactly
// the case the Go-aware transform removes before this engine ever runs. What
// is left -- a name table that gained entries, a pc table that gained rows,
// the inside of a function whose body genuinely changed -- is served by
// exact matching at a fraction of the memory and time.
//
// The index is keyed by content and ordered by position, so a lookup can ask
// "where near here?" and not just "where?". That distinction is the whole
// difference between a useful match and a useless one: in a 30 MB
// executable the same six bytes occur thousands of times, and only the
// occurrence near the caller's hint is the one the file actually moved.
package lz

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"sort"
)

// MinMatch is the shortest copy worth an op: a triple costs about three
// varint bytes, so anything shorter is cheaper as literals.
const MinMatch = 6

const (
	hashMul      = 0x9E3779B97F4A7C15
	defaultProbe = 5
)

// nearProbes are the entry offsets from the hint tried one by one.
var nearProbes = [...]int{1, 2, 3, 4, 5, 6, 7, 8}

// Index is a content index over a source buffer, grouped by hash bucket and
// ascending by position within a bucket.
type Index struct {
	src    []byte
	bucket []uint32 // prefix sums: bucket[b]..bucket[b+1] are b's entries
	pos    []uint32
	shift  uint
	probe  int
}

func hashBitsFor(n int) uint {
	return min(max(uint(bits.Len(uint(n))), 10), 22)
}

func (ix *Index) hash(b []byte) uint32 {
	v := binary.LittleEndian.Uint64(b) & 0xffff_ffff_ffff // the MinMatch leading bytes
	return uint32((v * hashMul) >> ix.shift)
}

// NewIndex indexes src. It costs four bytes per source byte plus the bucket
// table, and takes two linear passes -- no sort, because positions are
// placed in ascending order by construction.
func NewIndex(src []byte) *Index {
	hb := hashBitsFor(len(src))
	ix := &Index{src: src, shift: 64 - hb, probe: defaultProbe}
	n := len(src) - 7
	if n <= 0 {
		ix.bucket = make([]uint32, (1<<hb)+1)
		return ix
	}
	counts := make([]uint32, (1<<hb)+1)
	for i := 0; i < n; i++ {
		counts[ix.hash(src[i:])+1]++
	}
	for b := 1; b < len(counts); b++ {
		counts[b] += counts[b-1]
	}
	ix.bucket = counts
	fill := make([]uint32, 1<<hb)
	copy(fill, counts[:1<<hb])
	ix.pos = make([]uint32, n)
	for i := 0; i < n; i++ {
		h := ix.hash(src[i:])
		ix.pos[fill[h]] = uint32(i)
		fill[h]++
	}
	return ix
}

// SetProbe sets how many candidates on each side of the hint a lookup tries,
// at exponentially growing distance.
func (ix *Index) SetProbe(n int) { ix.probe = max(n, 1) }

// matchLen is the length of the common prefix of a and b.
func matchLen(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for ; i+8 <= n; i += 8 {
		if x := binary.LittleEndian.Uint64(a[i:]) ^ binary.LittleEndian.Uint64(b[i:]); x != 0 {
			return i + bits.TrailingZeros64(x)/8
		}
	}
	for ; i < n && a[i] == b[i]; i++ {
	}
	return i
}

// Find returns the longest source match for dst[p:] among the candidates
// nearest hint, which is where the caller believes the source has been
// tracking. A negative hint means no preference.
func (ix *Index) Find(dst []byte, p, hint int) (pos, length int) {
	if p+8 > len(dst) || len(ix.pos) == 0 {
		return 0, 0
	}
	if hint >= 0 && hint < len(ix.src) {
		if l := matchLen(ix.src[hint:], dst[p:]); l >= MinMatch {
			pos, length = hint, l
		}
	}
	b := ix.hash(dst[p:])
	ents := ix.pos[ix.bucket[b]:ix.bucket[b+1]]
	if len(ents) == 0 {
		if length < MinMatch {
			return 0, 0
		}
		return pos, length
	}
	want := hint
	if want < 0 {
		want = p
	}
	k := sort.Search(len(ents), func(i int) bool { return int(ents[i]) >= want })
	try := func(i int) {
		if i < 0 || i >= len(ents) {
			return
		}
		if l := matchLen(ix.src[ents[i]:], dst[p:]); l > length {
			pos, length = int(ents[i]), l
		}
	}
	// The nearest few entries linearly -- the match the file actually made
	// is usually one or two entries from the hint, and skipping those is
	// the difference between finding the shifted copy and not -- then
	// exponentially further out, so a bucket with thousands of entries
	// still costs a fixed number of probes and still reaches both ends.
	for _, d := range nearProbes {
		try(k - d)
		try(k + d - 1)
	}
	for step, n := len(nearProbes), 0; n < ix.probe; step, n = step*4, n+1 {
		try(k - step)
		try(k + step - 1)
	}
	try(0)
	try(len(ents) - 1)
	if length < MinMatch {
		return 0, 0
	}
	return pos, length
}

// Emit appends to ctrl and lit the ops that rebuild dst from ix's source.
// Each op is (literal count, copy count, signed source seek): emit that many
// literals, move the source cursor by seek, then copy that many bytes.
func (ix *Index) Emit(dst []byte, ctrl, lit []byte) (ctrlOut, litOut []byte) {
	var tmp [binary.MaxVarintLen64]byte
	putU := func(v uint64) { ctrl = append(ctrl, tmp[:binary.PutUvarint(tmp[:], v)]...) }
	putS := func(v int64) { ctrl = append(ctrl, tmp[:binary.PutVarint(tmp[:], v)]...) }

	cursor, p, litStart := 0, 0, 0
	for p < len(dst) {
		q, l := ix.Find(dst, p, cursor)
		if l == 0 {
			p++
			continue
		}
		// Lazy match: one byte later may start a longer copy, which is
		// worth the extra literal byte.
		if p+1 < len(dst) {
			if q2, l2 := ix.Find(dst, p+1, cursor); l2 > l+1 {
				p++
				q, l = q2, l2
			}
		}
		putU(uint64(p - litStart))
		putU(uint64(l))
		putS(int64(q - cursor))
		lit = append(lit, dst[litStart:p]...)
		cursor = q + l
		p += l
		litStart = p
	}
	if litStart < len(dst) {
		putU(uint64(len(dst) - litStart))
		putU(0)
		putS(0)
		lit = append(lit, dst[litStart:]...)
	}
	return ctrl, lit
}

// Emit indexes src and rebuilds dst from it in one call.
func Emit(src, dst, ctrl, lit []byte) (ctrlOut, litOut []byte) {
	return NewIndex(src).Emit(dst, ctrl, lit)
}

// Reader hands the ops of one op stream to Apply. Every read is checked
// against the streams and against the output buffer.
type Reader struct {
	Ctrl []byte
	Lit  []byte
}

// Apply rebuilds len(out) bytes into out from src and the reader's streams.
// It consumes exactly the ops it needs and leaves the rest, so one pair of
// streams can drive many calls.
func (r *Reader) Apply(src, out []byte) error {
	cursor, p := int64(0), 0
	for p < len(out) {
		nlit, n := binary.Uvarint(r.Ctrl)
		if n <= 0 {
			return fmt.Errorf("lz: truncated literal count")
		}
		r.Ctrl = r.Ctrl[n:]
		ncopy, n := binary.Uvarint(r.Ctrl)
		if n <= 0 {
			return fmt.Errorf("lz: truncated copy count")
		}
		r.Ctrl = r.Ctrl[n:]
		seek, n := binary.Varint(r.Ctrl)
		if n <= 0 {
			return fmt.Errorf("lz: truncated seek")
		}
		r.Ctrl = r.Ctrl[n:]

		if nlit > uint64(len(out)-p) || nlit > uint64(len(r.Lit)) {
			return fmt.Errorf("lz: %d literals with %d bytes left to write and %d in the stream", nlit, len(out)-p, len(r.Lit))
		}
		p += copy(out[p:], r.Lit[:nlit])
		r.Lit = r.Lit[nlit:]

		if ncopy == 0 {
			if seek != 0 {
				return fmt.Errorf("lz: seek without a copy")
			}
			if nlit == 0 {
				return fmt.Errorf("lz: empty op would not terminate")
			}
			continue
		}
		cursor += seek
		if cursor < 0 || ncopy > uint64(len(out)-p) || cursor+int64(ncopy) > int64(len(src)) {
			return fmt.Errorf("lz: copy of %d at source %d, source is %d bytes and %d remain to write", ncopy, cursor, len(src), len(out)-p)
		}
		p += copy(out[p:], src[cursor:cursor+int64(ncopy)])
		cursor += int64(ncopy)
	}
	return nil
}
