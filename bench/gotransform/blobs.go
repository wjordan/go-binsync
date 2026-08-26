package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// Stage 1 in two steps (Go 1.26/1.27 layouts):
//
//	1a: funcnametab and filetab are transmitted as a generic delta of the
//	    old tables (they are name lists; the delta is ~1.5 KB on prometheus).
//	1b: cutab, pctab and go:func.* are predicted from the old tables, the
//	    function layout and the content maps, and transmitted as a generic
//	    delta of that prediction (predictBlobs), which removes the cost of
//	    the embedded offsets (filetab offsets in cutab, funcnametab offsets
//	    in inline trees, rodata offsets in stack-object records) and of the
//	    re-layout in new function order.
//
// The rules emulated are those of cmd/link/internal/ld/pcln.go
// (generatePctab, generateFuncdata, generateFilenameTabs).

// pcTableLen returns the length of the self-delimiting pc-value table at
// off: (value delta, pc delta) varint pairs, terminated by a zero value
// delta (a zero is only legal in the first pair).
func pcTableLen(tab []byte, off int) int {
	p := off
	first := true
	for p < len(tab) {
		if tab[p] == 0 && !first {
			return p + 1 - off
		}
		for p < len(tab) && tab[p]&0x80 != 0 {
			p++
		}
		p++
		for p < len(tab) && tab[p]&0x80 != 0 {
			p++
		}
		p++
		first = false
	}
	return p - off
}

// blobPred is the predicted stage-1b blobs plus the old -> predicted offset
// maps that the _func records are re-based through.
type blobPred struct {
	Cutab, Pctab, Gofunc []byte
	PcOff                map[uint32]uint32 // old pctab offset -> predicted
	GfOff                map[uint32]uint32 // old go:func.* offset -> predicted
	Stats                blobStats
}

type blobStats struct {
	PcTables, PcDedup, FdSyms, InlNames, StkObjs, Wraps, CuEntries, CuMapped int
}

func (s blobStats) String() string {
	return fmt.Sprintf("pc tables=%d (dedup %d) funcdata syms=%d inline nameOff=%d stackobj gcdata=%d wrapinfo=%d cutab entries=%d mapped=%d", s.PcTables, s.PcDedup, s.FdSyms, s.InlNames, s.StkObjs, s.Wraps, s.CuEntries, s.CuMapped)
}

func (b *blobPred) concat() []byte {
	out := make([]byte, 0, len(b.Cutab)+len(b.Pctab)+len(b.Gofunc))
	out = append(out, b.Cutab...)
	out = append(out, b.Pctab...)
	return append(out, b.Gofunc...)
}

// stage1aBlobs / stage1bBlobs split the transmitted tables.
func stage1aBlobs(b *Bin) []byte {
	p := b.Pcln
	out := append([]byte(nil), p.Data[p.Funcnametab.Off:p.Funcnametab.End()]...)
	return append(out, p.Data[p.Filetab.Off:p.Filetab.End()]...)
}

func stage1bBlobs(b *Bin) []byte {
	p := b.Pcln
	out := append([]byte(nil), p.Data[p.Cutab.Off:p.Cutab.End()]...)
	out = append(out, p.Data[p.Pctab.Off:p.Pctab.End()]...)
	return append(out, gofuncBytes(b)...)
}

// fillSkeleton copies table data into the skeleton's fake pclntab.
func fillSkeleton(b *Bin, ranges []Range, data []byte) error {
	want := uint64(0)
	for _, r := range ranges {
		want += r.Len
	}
	if uint64(len(data)) != want {
		return fmt.Errorf("skeleton: blobs are %d bytes, layout implies %d", len(data), want)
	}
	o := uint64(0)
	for _, r := range ranges {
		copy(b.Pcln.Data[r.Off:r.End()], data[o:o+r.Len])
		o += r.Len
	}
	return nil
}

// nameOffMap maps old funcnametab offsets to new ones: through a content
// map of the two tables when the name at the mapped offset is the expected
// one, otherwise the k-th occurrence of the name in new for the k-th in old.
type nameOffMap struct {
	old, new []byte
	fnmap    *DataMap
	newOccs  map[string][]uint32
	oldOcc   map[uint32]int // old offset -> occurrence index of its name
}

func newNameOffMap(oldTab, newTab []byte) *nameOffMap {
	nm := &nameOffMap{old: oldTab, new: newTab, newOccs: map[string][]uint32{}, oldOcc: map[uint32]int{}}
	nm.fnmap = buildDataMapFuzzy(oldTab, newTab, 16, nil, 0)
	for off := 0; off < len(newTab); {
		e := bytes.IndexByte(newTab[off:], 0)
		if e < 0 {
			break
		}
		n := string(newTab[off : off+e])
		nm.newOccs[n] = append(nm.newOccs[n], uint32(off))
		off += e + 1
	}
	cnt := map[string]int{}
	for off := 0; off < len(oldTab); {
		e := bytes.IndexByte(oldTab[off:], 0)
		if e < 0 {
			break
		}
		n := string(oldTab[off : off+e])
		nm.oldOcc[uint32(off)] = cnt[n]
		cnt[n]++
		off += e + 1
	}
	return nm
}

func (nm *nameOffMap) nameAt(off uint32) (string, bool) {
	if int(off) >= len(nm.old) {
		return "", false
	}
	e := bytes.IndexByte(nm.old[off:], 0)
	if e < 0 {
		return "", false
	}
	return string(nm.old[off : int(off)+e]), true
}

func (nm *nameOffMap) Map(off uint32) (uint32, bool) {
	name, ok := nm.nameAt(off)
	if !ok {
		return off, false
	}
	c := nm.fnmap.Map(uint64(off))
	if c+uint64(len(name)) < uint64(len(nm.new)) && string(nm.new[c:c+uint64(len(name))]) == name && nm.new[c+uint64(len(name))] == 0 {
		return uint32(c), true
	}
	occs := nm.newOccs[name]
	if len(occs) == 0 {
		return off, false
	}
	k := nm.oldOcc[off]
	if k >= len(occs) {
		k = len(occs) - 1
	}
	return occs[k], true
}

// predictBlobs builds the stage-1b prediction. new is the skeleton (its
// function list is the new layout, its funcnametab/filetab are the 1a
// tables); mp maps old addresses into it.
func predictBlobs(old, new *Bin, m *Match, mp *mapper) *blobPred {
	op, np := old.Pcln, new.Pcln
	od := op.Data
	bp := &blobPred{PcOff: map[uint32]uint32{}, GfOff: map[uint32]uint32{}}
	st := &bp.Stats

	// ---- cutab: filetab offsets re-targeted by file name
	oldFiletab := od[op.Filetab.Off:op.Filetab.End()]
	newFiletab := np.Data[np.Filetab.Off:np.Filetab.End()]
	newFile := map[string]uint32{}
	for off := 0; off < len(newFiletab); {
		e := bytes.IndexByte(newFiletab[off:], 0)
		if e < 0 {
			break
		}
		if n := string(newFiletab[off : off+e]); newFile[n] == 0 {
			newFile[n] = uint32(off)
		}
		off += e + 1
	}
	oldCutab := od[op.Cutab.Off:op.Cutab.End()]
	bp.Cutab = append([]byte(nil), oldCutab...)
	for i := 0; i+4 <= len(oldCutab); i += 4 {
		e := binary.LittleEndian.Uint32(oldCutab[i:])
		if e == ^uint32(0) || int(e) >= len(oldFiletab) {
			continue
		}
		st.CuEntries++
		end := bytes.IndexByte(oldFiletab[e:], 0)
		if end < 0 {
			continue
		}
		if no, ok := newFile[string(oldFiletab[e:int(e)+end])]; ok || (end == 0 && e == 0) {
			binary.LittleEndian.PutUint32(bp.Cutab[i:], no)
			st.CuMapped++
		}
	}

	// ---- pctab: old tables in new function order, deduplicated by content
	oldPctab := od[op.Pctab.Off:op.Pctab.End()]
	pctab := make([]byte, 1, len(oldPctab))
	seen := map[string]uint32{}
	emitPc := func(off uint32) {
		if off == 0 || int(off) >= len(oldPctab) {
			return
		}
		if _, ok := bp.PcOff[off]; ok {
			return
		}
		l := pcTableLen(oldPctab, int(off))
		content := oldPctab[off : int(off)+l]
		if p, ok := seen[string(content)]; ok {
			bp.PcOff[off] = p
			st.PcDedup++
			return
		}
		p := uint32(len(pctab))
		seen[string(content)] = p
		bp.PcOff[off] = p
		pctab = append(pctab, content...)
		st.PcTables++
	}
	oft := od[op.Functab.Off:op.Functab.End()]
	for j := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		f := old.Funcs[i]
		npc, _, _ := op.funcRecord(f.FuncOff)
		rec := oft[f.FuncOff:]
		for _, o := range []int{16, 20, 24} { // pcsp, pcfile, pcln
			emitPc(binary.LittleEndian.Uint32(rec[o:]))
		}
		for k := 0; k < int(npc); k++ {
			if k == 2 { // PCDATA_InlTreeIndex: the pcinline table comes last
				continue
			}
			emitPc(binary.LittleEndian.Uint32(rec[funcSize+4*k:]))
		}
		if npc > 2 {
			emitPc(binary.LittleEndian.Uint32(rec[funcSize+4*2:]))
		}
	}
	bp.Pctab = pctab

	// ---- go:func.*: old funcdata symbols in new first-seen order, grouped
	// by decreasing alignment class, embedded offsets re-targeted
	oldGf := gofuncBytes(old)
	type fdSym struct {
		off   uint32
		size  uint32 // to the next symbol
		kind  int    // FUNCDATA index it is used as
		first int    // first old function using it
		class uint32
	}
	syms := map[uint32]*fdSym{}
	var offs []uint32
	for fi, f := range old.Funcs {
		npc, nfd, _ := op.funcRecord(f.FuncOff)
		rec := oft[f.FuncOff:]
		for k := 0; k < int(nfd); k++ {
			off := binary.LittleEndian.Uint32(rec[funcSize+4*int(npc)+4*k:])
			if off == ^uint32(0) || int(off) >= len(oldGf) {
				continue
			}
			if syms[off] == nil {
				syms[off] = &fdSym{off: off, kind: k, first: fi}
				offs = append(offs, off)
			}
		}
	}
	sort.Slice(offs, func(a, b int) bool { return offs[a] < offs[b] })
	// alignment class (the compiler sets it per symbol; the linker sorts the
	// blob by decreasing class, stable, so the old blob is a sequence of
	// regions each in first-use order): stack-object records 8, pointer
	// maps / inline trees / wrapinfo 4, the varint streams (opendefer,
	// arginfo, argliveinfo) 1 -- except the argument maps of assembly
	// functions, which are 8 and hence sit in the first region, before the
	// first break in first-use order. There is no padding within a region.
	firstBreak := len(offs)
	prevFirst := -1
	for k, off := range offs {
		if f := syms[off].first; f < prevFirst {
			firstBreak = k
			break
		} else {
			prevFirst = f
		}
	}
	for k, off := range offs {
		s := syms[off]
		if k+1 < len(offs) {
			s.size = offs[k+1] - off
		} else {
			s.size = uint32(len(oldGf)) - off
		}
		switch s.kind {
		case 2: // FUNCDATA_StackObjects
			s.class = 8
		case 0: // FUNCDATA_ArgsPointerMaps
			s.class = 4
			if k < firstBreak && s.size%8 == 0 {
				s.class = 8
			}
		case 1, 3, 7: // LocalsPointerMaps, InlTree, WrapInfo
			s.class = 4
		default: // OpenCodedDeferInfo, ArgInfo, ArgLiveInfo
			s.class = 1
		}
	}
	var classes [6][]*fdSym // 32, 16, 8, 4, 2, 1
	classIdx := func(c uint32) int {
		switch c {
		case 32:
			return 0
		case 16:
			return 1
		case 8:
			return 2
		case 4:
			return 3
		case 2:
			return 4
		}
		return 5
	}
	placed := map[uint32]bool{}
	for j := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		f := old.Funcs[i]
		npc, nfd, _ := op.funcRecord(f.FuncOff)
		rec := oft[f.FuncOff:]
		for k := 0; k < int(nfd); k++ {
			off := binary.LittleEndian.Uint32(rec[funcSize+4*int(npc)+4*k:])
			if s := syms[off]; s != nil && !placed[off] {
				placed[off] = true
				classes[classIdx(s.class)] = append(classes[classIdx(s.class)], s)
			}
		}
	}
	nmap := newNameOffMap(od[op.Funcnametab.Off:op.Funcnametab.End()], np.Data[np.Funcnametab.Off:np.Funcnametab.End()])
	om, nmod := old.Mod, new.Mod
	var gf []byte
	for _, cl := range classes {
		for _, s := range cl {
			a := uint64(s.class)
			for uint64(len(gf))%a != 0 {
				gf = append(gf, 0)
			}
			p := uint32(len(gf))
			bp.GfOff[s.off] = p
			gf = append(gf, oldGf[s.off:s.off+s.size]...)
			st.FdSyms++
			d := gf[p : p+s.size]
			switch s.kind {
			case 3: // inline tree: {funcID u8, pad[3], nameOff u32, parentPc u32, startLine u32}
				for e := 0; e+16 <= len(d); e += 16 {
					if no, ok := nmap.Map(binary.LittleEndian.Uint32(d[e+4:])); ok {
						binary.LittleEndian.PutUint32(d[e+4:], no)
						st.InlNames++
					}
				}
			case 2: // stack objects: n uintptr, then {off, size, ptrBytes int32, gcdataoff uint32}
				if len(d) >= 8 {
					n := binary.LittleEndian.Uint64(d)
					for r := uint64(0); r < n && 8+16*(r+1) <= uint64(len(d)); r++ {
						o := 8 + 16*r + 12
						v := binary.LittleEndian.Uint32(d[o:])
						if nv, cls := mp.mapAddr(om.Rodata+uint64(v), nil); cls == rcRodata {
							binary.LittleEndian.PutUint32(d[o:], uint32(nv-nmod.Rodata))
							st.StkObjs++
						}
					}
				}
			case 7: // wrapinfo: textOff of the wrapped function
				if len(d) >= 4 {
					v := binary.LittleEndian.Uint32(d)
					if nv, cls := mp.mapAddr(om.Text+uint64(v), nil); cls == rcTextMatched || cls == rcTextSelf || cls == rcTextNone {
						binary.LittleEndian.PutUint32(d, uint32(nv-nmod.Text))
						st.Wraps++
					}
				}
			}
		}
	}
	bp.Gofunc = gf
	// the linker pads each table to the alignment of the next symbol; the
	// layout carries the exact lengths, so pad (never truncate) to them
	pad := func(b []byte, n uint64) []byte {
		for uint64(len(b)) < n {
			b = append(b, 0)
		}
		return b
	}
	bp.Cutab = pad(bp.Cutab, new.Pcln.Cutab.Len)
	bp.Pctab = pad(bp.Pctab, new.Pcln.Pctab.Len)
	bp.Gofunc = pad(bp.Gofunc, new.Pcln.Gofunc.Len)
	return bp
}

// selfCheckBlobs predicts b's own blobs from itself and reports the
// differing bytes per table (0 means the linker's rules are reproduced).
func selfCheckBlobs(b *Bin) string {
	self := matchFuncs(b, b)
	mp := &mapper{src: b, dst: b, srcToDst: self.OldToNew}
	bp := predictBlobs(b, b, self, mp)
	p := b.Pcln
	trim := func(x []byte) []byte { return bytes.TrimRight(x, "\x00") }
	cu, _ := rawDiff(bp.Cutab, p.Data[p.Cutab.Off:p.Cutab.End()])
	pc, _ := rawDiff(bp.Pctab, trim(p.Data[p.Pctab.Off:p.Pctab.End()]))
	gf, _ := rawDiff(bp.Gofunc, trim(gofuncBytes(b)))
	return fmt.Sprintf("cutab %d B (%d differ), pctab %d vs %d B (%d differ), go:func.* %d vs %d B (%d differ); %s",
		len(bp.Cutab), cu, len(bp.Pctab), len(trim(p.Data[p.Pctab.Off:p.Pctab.End()])), pc, len(bp.Gofunc), len(trim(gofuncBytes(b))), gf, bp.Stats)
}
