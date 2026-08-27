package delta

import (
	"encoding/binary"
	"sort"
	"sync"

	"github.com/wjordan/go-binsync/delta/gobin"
	"github.com/wjordan/go-binsync/delta/x86"
)

// dataBlock is the content-map block size. 16 bytes is small enough to
// follow a single inserted string and large enough that a window is
// distinctive.
const dataBlock = 16

// predictDataSection re-lays an old data section's blocks through its
// content map and rewrites every absolute pointer through the mapper. dst
// must be zeroed and the new section's length.
func predictDataSection(old, new *gobin.Bin, name string, dm *dataMap, mp *mapper, dst []byte) {
	os := old.Sects[name]
	olo, ohi := old.ImageRange()
	block := dataBlock
	if dm != nil {
		block = dm.Block
	}
	delta := func(o int) int64 {
		if dm == nil || len(dm.Delta) == 0 {
			return 0
		}
		return dm.Delta[min(o/dm.Block, len(dm.Delta)-1)]
	}
	for o := 0; o < len(os.Data); o += block {
		e := min(o+block, len(os.Data))
		p := int64(o) + delta(o)
		if p < 0 || int(p)+(e-o) > len(dst) {
			continue
		}
		copy(dst[p:], os.Data[o:e])
	}
	for o := 0; o+8 <= len(os.Data); o += 8 {
		v := binary.LittleEndian.Uint64(os.Data[o:])
		if v < olo || v >= ohi {
			continue
		}
		p := int64(o) + delta(o)
		if p < 0 || int(p)+8 > len(dst) {
			continue
		}
		nv, cls := mp.mapAddr(v, nil)
		if cls == rcTextUnmatch || cls == rcOutside {
			continue
		}
		binary.LittleEndian.PutUint64(dst[p:], nv)
	}
}

// predictHoles fills the bytes of the new file that no allocated section
// covers. The ELF header and the program and section header tables sit
// before the first section and are left as the old file's, which is nearly
// right -- only the offsets and sizes that changed differ, a few dozen bytes.
// The gaps between sections are alignment padding and the linker leaves them
// zero, so they are cleared: the old file's leftovers there would otherwise
// make every section that moved pay for its gap. And the tail -- .shstrtab
// and the padding before it -- is not at the same offset in the two files,
// because the file's length changed, so it is copied from the old file's
// tail rather than left where it fell.
func predictHoles(pred []byte, old, new *gobin.Bin) {
	type span struct{ off, end int }
	var sp []span
	for _, s := range new.Order {
		if s.NoBits || s.Size == 0 || s.Off+s.Size > uint64(len(pred)) {
			continue
		}
		sp = append(sp, span{int(s.Off), int(s.Off + s.Size)})
	}
	if len(sp) == 0 {
		return
	}
	sort.Slice(sp, func(i, j int) bool { return sp[i].off < sp[j].off })
	for i := 1; i < len(sp); i++ {
		if sp[i-1].end < sp[i].off {
			clear(pred[sp[i-1].end:sp[i].off])
		}
	}
	oldEnd := 0
	for _, s := range old.Order {
		if !s.NoBits && s.Size != 0 {
			oldEnd = max(oldEnd, int(s.Off+s.Size))
		}
	}
	// The tail is padding then .shstrtab, and it is the section names that
	// are worth predicting, so the two tails are aligned at the end of the
	// file rather than at the start of the padding.
	tail := pred[sp[len(sp)-1].end:]
	clear(tail)
	if oldEnd < len(old.File) {
		src := old.File[oldEnd:]
		if len(src) > len(tail) {
			src = src[len(src)-len(tail):]
		}
		copy(tail[len(tail)-len(src):], src)
	}
}

// predictWhole builds the predicted new file. new is the skeleton -- the
// section table, the moduledata values, the function list and the stage-1
// tables -- and nothing else of the real new binary is read, which is what
// lets the decoder run this identically. The base is a copy of the old file,
// so the ELF and program headers come for free; then the bytes no allocated
// section covers are fixed up, and every allocated section is overwritten
// with its prediction.
func predictWhole(old, new *gobin.Bin, l *layout, mp *mapper, st *x86.Stats) []byte {
	pred := make([]byte, l.NewLen)
	copy(pred, old.File)
	predictHoles(pred, old, new)
	ptrOnly := map[string]bool{}
	for _, n := range ptrRewriteSects {
		ptrOnly[n] = true
	}
	typeSect := ""
	if s := old.SectionOf(old.Mod.Types); s != nil {
		typeSect = s.Name
	}
	var wg sync.WaitGroup
	for _, ns := range new.Order {
		if ns.NoBits || ns.Off+ns.Size > uint64(len(pred)) {
			continue
		}
		dst := pred[ns.Off : ns.Off+ns.Size]
		clear(dst)
		os := old.Sects[ns.Name]
		wg.Add(1)
		go func(ns *gobin.Section, dst []byte) {
			defer wg.Done()
			switch {
			case ns.Name == ".text":
				predictText(mp, dst, st)
			case ns.Name == ".gopclntab":
				copy(dst, predictPcln(old, new, mp.m, l, mp.blobs))
			case os == nil:
				// a section this release introduced: nothing to predict from
			case ns.Name == ".plt":
				// PLT stubs are code: rip-relative jmp/push into .got.plt
				x86.Relocate(os.Data, dst, os.Addr, ns.Addr, mp.lookup(nil), &x86.Stats{}, nil)
			case mp.dataMaps[ns.Name] != nil:
				predictDataSection(old, new, ns.Name, mp.dataMaps[ns.Name], mp, dst)
				if ns.Name == typeSect {
					rewriteTypeOffsets(old, new, ns.Name, mp.dataMaps[ns.Name], mp, dst, nil)
				}
			case ptrOnly[ns.Name]:
				predictDataSection(old, new, ns.Name, nil, mp, dst)
			default:
				copy(dst, os.Data)
			}
		}(ns, dst)
	}
	wg.Wait()
	return pred
}

// buildMaps is the encoder side of the content maps and the shift tables.
func buildMaps(old, new *gobin.Bin, m *match) (map[string]*dataMap, map[string]*shiftTable) {
	dmaps := map[string]*dataMap{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range dataMapSects {
		if old.Sects[n] == nil || new.Sects[n] == nil || old.Sects[n].NoBits {
			continue
		}
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			dm := buildSectionMap(old, new, n, dataBlock)
			mu.Lock()
			dmaps[n] = dm
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	return dmaps, deriveShiftTables(old, new, m)
}

// deriveOverrides is the encoder-side target vote, and the only place the
// encoder looks at the new binary to correct its own maps.
//
// Every reference whose new position is known -- an absolute pointer in a
// data section, or a nameOff/typeOff/textOff field of a type descriptor, in
// a block the content map matched or placed in a stable run -- is read back
// from the new binary at that position. The value there is that reference's
// true new target. The votes are used twice:
//
//  1. a block the content map could not match, whose targets agree on a
//     shift other than the map's, gets that shift (transmitted through the
//     RLE map, and propagated over the unmatched blocks that follow); with
//     better positions a second round collects more votes;
//  2. a target the majority still mispredicts gets an explicit override,
//     old address to new, when at least ovMinVotes references agree.
//
// This resolves the targets a content map cannot place by content -- short,
// repetitive symbols such as runtime.gcbits.* that match at several shifts
// -- and the descriptors whose only change is an embedded offset.
const (
	ovMinVotes = 2
	ovRounds   = 2
)

type vote struct{ o, n uint64 }

// majority is one old target's winning new target, with the winning and
// total vote counts.
type majority struct {
	o, n         uint64
	count, total int
}

func collectVotes(old, new *gobin.Bin, dmaps map[string]*dataMap, mp *mapper) []vote {
	olo, ohi := old.ImageRange()
	nlo, nhi := new.ImageRange()
	var votes []vote
	for _, name := range append(append([]string{}, dataMapSects...), ptrRewriteSects...) {
		os, ns := old.Sects[name], new.Sects[name]
		if os == nil || ns == nil || os.NoBits || ns.NoBits {
			continue
		}
		dm := dmaps[name]
		for o := 0; o+8 <= len(os.Data); o += 8 {
			v := binary.LittleEndian.Uint64(os.Data[o:])
			if v < olo || v >= ohi {
				continue
			}
			p := int64(o)
			if dm != nil {
				i := o / dm.Block
				if !dm.reliable(i) {
					continue
				}
				p += dm.Delta[i]
			}
			if p < 0 || int(p)+8 > len(ns.Data) {
				continue
			}
			nv := binary.LittleEndian.Uint64(ns.Data[p:])
			if nv < nlo || nv >= nhi {
				continue
			}
			votes = append(votes, vote{v, nv})
		}
	}
	if tsec := old.SectionOf(old.Mod.Types); tsec != nil && dmaps[tsec.Name] != nil && new.Sects[tsec.Name] != nil {
		dm := dmaps[tsec.Name]
		nd := new.Sects[tsec.Name].Data
		rewriteTypeOffsets(old, new, tsec.Name, dm, mp, nil, func(off, target uint64, kind byte) {
			i := int(off) / dm.Block
			if !dm.reliable(i) {
				return
			}
			p := int64(off) + dm.Delta[i]
			if p < 0 || int(p)+4 > len(nd) {
				return
			}
			v := uint64(binary.LittleEndian.Uint32(nd[p:]))
			nt := new.Mod.Types + v
			if kind == 'x' {
				nt = new.Mod.Text + v
			}
			if nt < nlo || nt >= nhi {
				return
			}
			votes = append(votes, vote{target, nt})
		})
	}
	sort.Slice(votes, func(a, b int) bool {
		if votes[a].o != votes[b].o {
			return votes[a].o < votes[b].o
		}
		return votes[a].n < votes[b].n
	})
	return votes
}

// majorities reduces sorted votes to one entry per old target.
func majorities(votes []vote) []majority {
	var out []majority
	for i := 0; i < len(votes); {
		j := i
		var best uint64
		bestN := 0
		for j < len(votes) && votes[j].o == votes[i].o {
			k := j
			for k < len(votes) && votes[k].n == votes[j].n {
				k++
			}
			if k-j > bestN {
				best, bestN = votes[j].n, k-j
			}
			j = k
		}
		out = append(out, majority{votes[i].o, best, bestN, j - i})
		i = j
	}
	return out
}

func deriveOverrides(old, new *gobin.Bin, m *match, dmaps map[string]*dataMap, shifts map[string]*shiftTable) []addrOverride {
	mp := &mapper{src: old, dst: new, srcToDst: m.OldToNew, dstToSrc: m.NewToOld,
		m: m, dataMaps: dmaps, shifts: shifts}
	var maj []majority
	for range ovRounds {
		maj = majorities(collectVotes(old, new, dmaps, mp))
		fixBlocks(old, new, dmaps, maj)
	}
	var out []addrOverride
	for _, mj := range maj {
		pred, cls := mp.mapAddr(mj.o, nil)
		if cls == rcOutside || cls == rcTextUnmatch || mj.n == pred {
			continue
		}
		if mj.count >= ovMinVotes && 2*mj.count > mj.total {
			out = append(out, addrOverride{mj.o, mj.n})
		}
	}
	return out
}

// fixBlocks moves the unmatched blocks whose targets agree on a different
// shift, then re-propagates the shift over the plain unmatched blocks that
// follow -- as the forward pass did with the shift it had.
func fixBlocks(old, new *gobin.Bin, dmaps map[string]*dataMap, maj []majority) {
	type bk struct {
		name string
		i    int
	}
	bvotes := map[bk]map[int64]int{}
	for _, mj := range maj {
		if 2*mj.count <= mj.total {
			continue
		}
		s := old.SectionOf(mj.o)
		if s == nil {
			continue
		}
		dm, ns := dmaps[s.Name], new.Sects[s.Name]
		if dm == nil || ns == nil {
			continue
		}
		i := int(mj.o-s.Addr) / dm.Block
		if i >= len(dm.Delta) || dm.Matched[i] {
			continue
		}
		k := bk{s.Name, i}
		if bvotes[k] == nil {
			bvotes[k] = map[int64]int{}
		}
		bvotes[k][int64(mj.n-ns.Addr)-int64(mj.o-s.Addr)] += mj.count
	}
	fixed := map[string][]bool{}
	for k, vs := range bvotes {
		dm := dmaps[k.name]
		var best int64
		bestN, total := 0, 0
		for d, c := range vs {
			total += c
			if c > bestN || c == bestN && d < best {
				best, bestN = d, c
			}
		}
		if 2*bestN <= total || best == dm.Delta[k.i] {
			continue
		}
		if fixed[k.name] == nil {
			fixed[k.name] = make([]bool, len(dm.Delta))
		}
		dm.Delta[k.i] = best
		fixed[k.name][k.i] = true
	}
	for name, fx := range fixed {
		dm := dmaps[name]
		var cur int64
		for i := range dm.Delta {
			switch {
			case dm.Matched[i] || fx[i]:
				cur = dm.Delta[i]
			case dm.Ambiguous[i]:
				// the backward pass placed this one; leave it
			default:
				dm.Delta[i] = cur
			}
		}
	}
}
