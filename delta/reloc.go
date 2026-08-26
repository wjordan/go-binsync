package delta

import (
	"sync"
	"sync/atomic"

	"binsync/delta/gobin"
	"binsync/delta/x86"
)

// predictWorkers is the fan-out of every prediction pass. It is a constant
// and not a core count on purpose: the decoder runs the same passes, and a
// machine with a different number of cores must produce the same bytes.
const predictWorkers = 24

// refClass says what kind of thing an old address pointed at, which is what
// decides whether a mispredicted target is worth an override and whether a
// relocation may be attempted at all.
type refClass uint8

const (
	rcTextSelf    refClass = iota // inside the referring function
	rcTextMatched                 // another function that exists in the new binary
	rcTextUnmatch                 // an old function with no counterpart
	rcTextNone                    // in .text but not inside any function
	rcData                        // any allocated data section
	rcOutside                     // not in the image, or a section the new file lost
)

// mapper turns an absolute address of the old binary into the address the
// same thing has in the new one. It is the single point through which every
// predictor -- instruction operands, data pointers, type-descriptor offsets
// -- asks "where did this go", so all of them agree by construction.
type mapper struct {
	src, dst  *gobin.Bin
	srcToDst  []int // old function index -> new, or -1
	dstToSrc  []int // new function index -> old, or -1
	m         *match
	dataMaps  map[string]*dataMap
	shifts    map[string]*shiftTable
	overrides map[uint64]uint64
	blobs     *blobPred
}

func (mp *mapper) mapAddr(t uint64, self *gobin.Func) (uint64, refClass) {
	a, cls := mp.mapAddrBase(t, self)
	if nv, ok := mp.overrides[t]; ok {
		return nv, cls
	}
	return a, cls
}

// mapAddrBase is mapAddr through the function map, the content maps and the
// shift tables only. deriveOverrides needs it separately, to ask what the
// maps alone would have predicted.
func (mp *mapper) mapAddrBase(t uint64, self *gobin.Func) (uint64, refClass) {
	src, dst := mp.src, mp.dst
	if t >= src.Text.Addr && t < src.Text.Addr+src.Text.Size {
		f := src.FuncAt(t)
		if f == nil {
			return dst.Text.Addr + (t - src.Text.Addr), rcTextNone
		}
		j := mp.srcToDst[f.Idx]
		if j < 0 {
			return 0, rcTextUnmatch
		}
		g := dst.Funcs[j]
		if f == self {
			return g.Entry + (t - f.Entry), rcTextSelf
		}
		return g.Entry + (t - f.Entry), rcTextMatched
	}
	s := src.SectionOf(t)
	if s == nil && t > 0 {
		s = src.SectionOf(t - 1) // one-past-the-end references
	}
	if s == nil {
		return 0, rcOutside
	}
	ds := dst.Sects[s.Name]
	if ds == nil {
		return 0, rcOutside
	}
	off := t - s.Addr
	if dm := mp.dataMaps[s.Name]; dm != nil {
		off = dm.Map(off)
	} else if st := mp.shifts[s.Name]; st != nil {
		off = st.Map(off)
	}
	return ds.Addr + off, rcData
}

// lookup adapts the mapper to the x86 relocator, which only wants to know
// whether an address has a new home and where.
func (mp *mapper) lookup(self *gobin.Func) func(uint64) x86.Target {
	return func(t uint64) x86.Target {
		a, cls := mp.mapAddr(t, self)
		if cls == rcTextUnmatch || cls == rcOutside {
			return x86.Target{}
		}
		return x86.Target{Addr: a, Known: true}
	}
}

// predictText writes the predicted new .text into out, which must be the new
// .text's length. Every matched function's bytes are copied to its new
// address with its PC-relative operands re-targeted; a function with no old
// counterpart is left as INT3 for the correction to fill in, and so is the
// tail padding of a function that grew.
func predictText(mp *mapper, out []byte, st *x86.Stats) {
	src, dst := mp.src, mp.dst
	for i := range out {
		out[i] = 0xCC
	}
	// the linker's prologue before the first function and whatever follows
	// the last are copied verbatim
	copy(out, src.Text.Data[:src.Funcs[0].Entry-src.Text.Addr])
	copy(out[dst.Funcs[len(dst.Funcs)-1].End-dst.Text.Addr:],
		src.Text.Data[src.Funcs[len(src.Funcs)-1].End-src.Text.Addr:])

	stats := make([]x86.Stats, predictWorkers)
	var wg sync.WaitGroup
	var next atomic.Int64
	for w := range predictWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				j := int(next.Add(1)) - 1
				if j >= len(dst.Funcs) {
					return
				}
				i := mp.dstToSrc[j]
				if i < 0 {
					continue
				}
				g := dst.Funcs[j]
				f := src.Funcs[i]
				lo := g.Entry - dst.Text.Addr
				x86.Relocate(src.FuncBytes(f), out[lo:lo+g.Size()], f.Entry, g.Entry, mp.lookup(f), &stats[w], nil)
			}
		}(w)
	}
	wg.Wait()
	if st != nil {
		for _, s := range stats {
			st.Add(s)
		}
	}
}
