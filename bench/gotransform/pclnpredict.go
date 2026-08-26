package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
)

const (
	subbucketSize = 256
	bucketSize    = 4096
	subbuckets    = 16
)

// genFindfunctab regenerates runtime.findfunctab from the function table
// (cmd/link/internal/ld/pcln.go findfunctab).
func genFindfunctab(funcs []*Func) []byte {
	min := funcs[0].Entry
	max := funcs[len(funcs)-1].End
	n := int((max - min + subbucketSize - 1) / subbucketSize)
	nb := int((max - min + bucketSize - 1) / bucketSize)
	idx := make([]int32, n)
	for i := range idx {
		idx[i] = -1
	}
	set := func(i int, v int32) {
		if idx[i] < 0 || idx[i] > v {
			idx[i] = v
		}
	}
	for fi, f := range funcs {
		q := f.End
		for p := f.Entry; p < q; p += subbucketSize {
			set(int((p-min)/subbucketSize), int32(fi))
		}
		set(int((q-1-min)/subbucketSize), int32(fi))
	}
	out := make([]byte, 4*nb+n)
	for i := 0; i < nb; i++ {
		base := idx[i*subbuckets]
		binary.LittleEndian.PutUint32(out[i*(4+subbuckets):], uint32(base))
		for j := 0; j < subbuckets && i*subbuckets+j < n; j++ {
			out[i*(4+subbuckets)+4+j] = byte(idx[i*subbuckets+j] - base)
		}
	}
	return out
}

// nameSegments models how the linker fills funcnametab (walkFuncs order):
// walking functions in functab order, a function's name is emitted when first
// seen, followed by the names of its inlined callees not seen before. A
// function whose name was already emitted (as somebody's inlined callee)
// contributes nothing. So the table is a sequence of segments, one per
// "starter" function; a starter is a function whose nameOff exceeds every
// nameOff seen so far in functab order.
// Returns per old function index: the segment range it starts (Len 0 if not a starter).
func nameSegments(b *Bin) []Range {
	segs := make([]Range, len(b.Funcs))
	maxOff := int32(-1)
	starters := []int{}
	for i, f := range b.Funcs {
		if f.NameOff > maxOff {
			maxOff = f.NameOff
			starters = append(starters, i)
		}
	}
	end := uint64(b.Pcln.Funcnametab.Len)
	for k, i := range starters {
		e := end
		if k+1 < len(starters) {
			e = uint64(b.Funcs[starters[k+1]].NameOff)
		}
		segs[i] = Range{uint64(b.Funcs[i].NameOff), e - uint64(b.Funcs[i].NameOff)}
	}
	return segs
}

type pclnVariant struct {
	name        string
	rebuildName bool // rebuild funcnametab in new layout order
	oracleTabs  bool // use NEW pctab/go:func.* and re-base old offsets through content maps
	oracleNames bool // use NEW funcnametab/cutab/filetab (transmitted as their own deltas); nameOff by name lookup
}

var gfBlock = 16 // block size for the pctab / go:func.* content maps
var gfTol = 4    // fuzzy tolerance (differing bytes) for those maps

func rnd(x, a uint64) uint64 { return (x + a - 1) / a * a }

// gofuncBytes returns the go:func.* blob (inside .gopclntab for Go 1.26,
// inside .rodata for Go 1.25).
func gofuncBytes(b *Bin) []byte {
	p := b.Pcln
	if p.Inside {
		return p.Data[p.Gofunc.Off:p.Gofunc.End()]
	}
	ro := b.Sects[".rodata"].Data
	return ro[p.Gofunc.Off:p.Gofunc.End()]
}

func findfunctabBytes(b *Bin) []byte {
	p := b.Pcln
	if p.Inside {
		return p.Data[p.Findfunctab.Off:p.Findfunctab.End()]
	}
	ro := b.Sects[".rodata"].Data
	return ro[p.Findfunctab.Off:p.Findfunctab.End()]
}

// predictPcln builds a predicted new .gopclntab from old + layout (new
// function order, names, sizes).
func predictPcln(old, new *Bin, m *Match, v pclnVariant, lay *Layout, bp *blobPred) []byte {
	op := old.Pcln
	np := new.Pcln
	od := op.Data
	nfunc := len(new.Funcs)
	// transmitted record shapes (npcdata, nfuncdata) override the defaults
	shapes := map[int][2]uint32{}
	if lay != nil {
		for _, r := range lay.RecShapes {
			shapes[r.Idx] = [2]uint32{r.Npcdata, r.Nfuncdata}
		}
	}

	// 1. funcnametab
	var fnt []byte
	nameOff := make([]int32, nfunc)
	if v.oracleNames {
		fnt = append(fnt, np.Data[np.Funcnametab.Off:np.Funcnametab.End()]...)
		// name -> offsets in the new table, in order of occurrence
		byName := map[string][]int32{}
		for off := 0; off < len(fnt); {
			e := bytes.IndexByte(fnt[off:], 0)
			if e < 0 {
				break
			}
			n := string(fnt[off : off+e])
			byName[n] = append(byName[n], int32(off))
			off += e + 1
		}
		// A matched function's old nameOff is mapped through an old->new
		// content map of funcnametab and accepted when the name at the
		// mapped offset is the expected one; this reproduces the linker's
		// choice among identical name strings (inlined-only twins produce
		// extra entries). Anything else falls back to the by-name lookup,
		// in old nameOff order within a name group.
		fnmap := buildDataMapFuzzy(od[op.Funcnametab.Off:op.Funcnametab.End()], fnt, 16, nil, 0)
		taken := map[int32]bool{}
		groups := map[string][]int{}
		for j, g := range new.Funcs {
			name := g.Name
			if i := m.NewToOld[j]; i >= 0 {
				name = old.Funcs[i].Name
				c := fnmap.Map(uint64(old.Funcs[i].NameOff))
				if c+uint64(len(name)) < uint64(len(fnt)) && string(fnt[c:c+uint64(len(name))]) == name && fnt[c+uint64(len(name))] == 0 && !taken[int32(c)] {
					nameOff[j] = int32(c)
					taken[int32(c)] = true
					continue
				}
			}
			groups[name] = append(groups[name], j)
		}
		for name, js := range groups {
			sort.SliceStable(js, func(a, b int) bool {
				ia, ib := m.NewToOld[js[a]], m.NewToOld[js[b]]
				if ia < 0 || ib < 0 {
					return ia >= 0 && ib < 0
				}
				return old.Funcs[ia].NameOff < old.Funcs[ib].NameOff
			})
			var offs []int32
			for _, o := range byName[name] {
				if !taken[o] {
					offs = append(offs, o)
				}
			}
			if len(offs) == 0 {
				offs = byName[name]
			}
			for k, j := range js {
				switch {
				case k < len(offs):
					nameOff[j] = offs[k]
				case len(offs) > 0:
					nameOff[j] = offs[len(offs)-1]
				default:
					nameOff[j] = 0
				}
			}
		}
	} else if v.rebuildName {
		segs := nameSegments(old)
		base := op.Funcnametab.Off
		// placed[oldSegStart] = new offset of that segment
		placed := map[uint64]uint64{}
		segOf := make([]int, len(old.Funcs)) // old func -> starter index whose segment contains its name
		{
			starters := []int{}
			for i := range old.Funcs {
				if segs[i].Len > 0 {
					starters = append(starters, i)
				}
			}
			for i, f := range old.Funcs {
				k := sort.Search(len(starters), func(k int) bool { return old.Funcs[starters[k]].NameOff > f.NameOff }) - 1
				segOf[i] = starters[k]
			}
		}
		for j, g := range new.Funcs {
			i := m.NewToOld[j]
			if i < 0 {
				nameOff[j] = int32(len(fnt))
				fnt = append(fnt, g.Name...)
				fnt = append(fnt, 0)
				continue
			}
			if r := segs[i]; r.Len > 0 {
				placed[r.Off] = uint64(len(fnt))
				fnt = append(fnt, od[base+r.Off:base+r.Off+r.Len]...)
			}
		}
		for j := range new.Funcs {
			i := m.NewToOld[j]
			if i < 0 {
				continue
			}
			s := segs[segOf[i]]
			if p, ok := placed[s.Off]; ok {
				nameOff[j] = int32(p + uint64(old.Funcs[i].NameOff) - s.Off)
			} else {
				nameOff[j] = int32(len(fnt))
				fnt = append(fnt, old.Funcs[i].Name...)
				fnt = append(fnt, 0)
			}
		}
	} else {
		fnt = append(fnt, od[op.Funcnametab.Off:op.Funcnametab.End()]...)
		for j, g := range new.Funcs {
			i := m.NewToOld[j]
			if i < 0 {
				nameOff[j] = int32(len(fnt))
				fnt = append(fnt, g.Name...)
				fnt = append(fnt, 0)
			} else {
				nameOff[j] = old.Funcs[i].NameOff
			}
		}
	}
	cutab := od[op.Cutab.Off:op.Cutab.End()]
	filetab := od[op.Filetab.Off:op.Filetab.End()]
	var cuMap *DataMap
	if v.oracleNames {
		cutab = np.Data[np.Cutab.Off:np.Cutab.End()]
		filetab = np.Data[np.Filetab.Off:np.Filetab.End()]
		if bp != nil {
			cuMap = buildDataMap(bp.Cutab, cutab, 16)
		} else {
			cuMap = buildDataMap(od[op.Cutab.Off:op.Cutab.End()], cutab, 16)
		}
	}
	pctab := od[op.Pctab.Off:op.Pctab.End()]
	gofuncOld, gofuncNew := gofuncBytes(old), gofuncBytes(new)
	gofunc := gofuncOld
	// old table offset -> new: with a stage-1b prediction, old -> predicted
	// (exact, from the emulated layout) then predicted -> new through a
	// content map of two nearly identical blobs; otherwise old -> new
	mapPc := func(x uint32) uint32 { return x }
	mapGf := mapPc
	if v.oracleTabs {
		pctab = np.Data[np.Pctab.Off:np.Pctab.End()]
		gofunc = gofuncNew
		if bp != nil {
			pcmap := buildDataMapFuzzy(bp.Pctab, pctab, gfBlock, nil, gfTol)
			gfmap := buildDataMapFuzzy(bp.Gofunc, gofuncNew, gfBlock, nil, gfTol)
			mapPc = func(x uint32) uint32 {
				if p, ok := bp.PcOff[x]; ok {
					return uint32(pcmap.Map(uint64(p)))
				}
				return x
			}
			mapGf = func(x uint32) uint32 {
				if p, ok := bp.GfOff[x]; ok {
					return uint32(gfmap.Map(uint64(p)))
				}
				return x
			}
		} else {
			pcmap := buildDataMapFuzzy(od[op.Pctab.Off:op.Pctab.End()], pctab, gfBlock, nil, gfTol)
			gfmap := buildDataMapFuzzy(gofuncOld, gofuncNew, gfBlock, nil, gfTol)
			mapPc = func(x uint32) uint32 { return uint32(pcmap.Map(uint64(x))) }
			mapGf = func(x uint32) uint32 { return uint32(gfmap.Map(uint64(x))) }
		}
	}

	// 2. functab + _func records
	// mode of (npcdata, nfuncdata) among old records, for new functions
	modeKey := modalShape(old)
	oft := od[op.Functab.Off:op.Functab.End()]
	hdr := uint64(nfunc*8 + 4)
	recOff := make([]uint64, nfunc)
	recs := make([][]byte, nfunc)
	size := hdr
	var prevCU uint32
	for j, g := range new.Funcs {
		i := m.NewToOld[j]
		var rec []byte
		entryOff := uint32(g.Entry - new.Mod.Text)
		if i >= 0 {
			f := old.Funcs[i]
			npc, nfd, sz := op.funcRecord(f.FuncOff)
			rec = append([]byte(nil), oft[f.FuncOff:f.FuncOff+sz]...)
			if v.oracleTabs {
				for _, o := range []int{16, 20, 24} { // pcsp, pcfile, pcln
					if x := binary.LittleEndian.Uint32(rec[o:]); x != 0 {
						binary.LittleEndian.PutUint32(rec[o:], mapPc(x))
					}
				}
				for k := 0; k < int(npc); k++ {
					o := funcSize + 4*k
					if x := binary.LittleEndian.Uint32(rec[o:]); x != 0 {
						binary.LittleEndian.PutUint32(rec[o:], mapPc(x))
					}
				}
				for k := 0; k < int(nfd); k++ {
					o := funcSize + 4*int(npc) + 4*k
					if x := binary.LittleEndian.Uint32(rec[o:]); x != ^uint32(0) {
						binary.LittleEndian.PutUint32(rec[o:], mapGf(x))
					}
				}
			}
			if cuMap != nil {
				if x := binary.LittleEndian.Uint32(rec[32:]); x != ^uint32(0) {
					binary.LittleEndian.PutUint32(rec[32:], uint32(cuMap.Map(uint64(x*4))/4))
				}
			}
			if sh, ok := shapes[j]; ok && sh != [2]uint32{npc, nfd} {
				// the record changed shape: keep the header, copy what
				// exists of the pcdata/funcdata arrays, fill the rest
				nr := make([]byte, funcSize+4*sh[0]+4*sh[1])
				copy(nr, rec[:funcSize])
				for k := uint32(0); k < sh[0]; k++ {
					if k < npc {
						copy(nr[funcSize+4*k:], rec[funcSize+4*k:funcSize+4*k+4])
					}
				}
				for k := uint32(0); k < sh[1]; k++ {
					o := funcSize + 4*sh[0] + 4*k
					if k < nfd {
						copy(nr[o:], rec[funcSize+4*npc+4*k:funcSize+4*npc+4*k+4])
					} else {
						binary.LittleEndian.PutUint32(nr[o:], ^uint32(0))
					}
				}
				binary.LittleEndian.PutUint32(nr[28:], sh[0])
				nr[43] = byte(sh[1])
				rec = nr
			}
			prevCU = binary.LittleEndian.Uint32(rec[32:])
		} else {
			npc, nfd := modeKey[0], modeKey[1]
			if sh, ok := shapes[j]; ok {
				npc, nfd = sh[0], sh[1]
			}
			rec = make([]byte, funcSize+4*npc+4*nfd)
			binary.LittleEndian.PutUint32(rec[28:], npc)
			binary.LittleEndian.PutUint32(rec[32:], prevCU)
			rec[43] = byte(nfd)
			for k := 0; k < int(nfd); k++ {
				binary.LittleEndian.PutUint32(rec[funcSize+4*int(npc)+4*k:], ^uint32(0))
			}
		}
		binary.LittleEndian.PutUint32(rec[0:], entryOff)
		binary.LittleEndian.PutUint32(rec[4:], uint32(nameOff[j]))
		size = rnd(size, 8)
		recOff[j] = size
		recs[j] = rec
		size += uint64(len(rec))
	}
	functab := make([]byte, size)
	for j, g := range new.Funcs {
		binary.LittleEndian.PutUint32(functab[8*j:], uint32(g.Entry-new.Mod.Text))
		binary.LittleEndian.PutUint32(functab[8*j+4:], uint32(recOff[j]))
		copy(functab[recOff[j]:], recs[j])
	}
	binary.LittleEndian.PutUint32(functab[8*nfunc:], uint32(new.Funcs[nfunc-1].End-new.Mod.Text))

	// 3. findfunctab
	fft := genFindfunctab(new.Funcs)

	// 4. assemble: header, funcnametab, cutab, filetab, pctab, functab, go:func.*, findfunctab
	// Alignments from cmd/link/internal/ld/pcln.go: funcnametab 1, cutab 4, filetab 1,
	// pctab 1, functab 4, go:func.* maxAlign (16 observed), findfunctab 4.
	// The blob tables carry their own trailing alignment padding (ranges run
	// to the next table), so they are placed back to back; the rebuilt
	// functab is 4-aligned and go:func.*/findfunctab 16/4-aligned as in the
	// linker.
	// With a transmitted layout the tables are placed at the new file's
	// actual offsets (go:func.*'s alignment is data-dependent: 16 on the
	// Go 1.26 builds, 8 on the 1.27 ones), which makes the prediction
	// length-exact; without one the linker's alignments are assumed.
	var out []byte
	out = append(out, od[:op.FuncnameOff]...) // header (+ padding) copied, patched below
	place := func(tab []byte, a uint64, at uint64) uint64 {
		if lay != nil {
			if uint64(len(out)) > at {
				fmt.Fprintf(os.Stderr, "WARN: pclntab table overruns transmitted offset by %d bytes\n", uint64(len(out))-at)
				out = out[:at]
			}
			for uint64(len(out)) < at {
				out = append(out, 0)
			}
		} else {
			for uint64(len(out))%a != 0 {
				out = append(out, 0)
			}
		}
		off := uint64(len(out))
		out = append(out, tab...)
		return off
	}
	var t [7]uint64
	if lay != nil {
		t = lay.TabOff
	}
	fnOff := place(fnt, 1, t[0])
	cuOff := place(cutab, 1, t[1])
	ftOff := place(filetab, 1, t[2])
	pcOff := place(pctab, 1, t[3])
	fnctabOff := place(functab, 4, t[4])
	if op.Inside {
		place(gofunc, 16, t[5])
		place(fft, 4, t[6])
	}
	if lay != nil {
		for uint64(len(out)) < lay.PclnLen {
			out = append(out, 0)
		}
		if uint64(len(out)) > lay.PclnLen {
			fmt.Fprintf(os.Stderr, "WARN: predicted pclntab %d bytes > transmitted %d\n", len(out), lay.PclnLen)
			out = out[:lay.PclnLen]
		}
		binary.LittleEndian.PutUint64(out[16:], lay.NFiles)
	}
	binary.LittleEndian.PutUint64(out[8:], uint64(nfunc))
	binary.LittleEndian.PutUint64(out[32:], fnOff)
	binary.LittleEndian.PutUint64(out[40:], cuOff)
	binary.LittleEndian.PutUint64(out[48:], ftOff)
	binary.LittleEndian.PutUint64(out[56:], pcOff)
	binary.LittleEndian.PutUint64(out[64:], fnctabOff)
	return out
}

func cmdPclnPredict(args []string) {
	fs := flag.NewFlagSet("pclnpredict", flag.ExitOnError)
	which := fs.String("enc", "bhz", "encoders")
	fs.IntVar(&gfBlock, "gfblock", 16, "block size for pctab/go:func.* content maps")
	fs.IntVar(&gfTol, "gftol", 4, "fuzzy tolerance for pctab/go:func.* content maps")
	fs.Parse(args)
	if fs.NArg() < 2 {
		fatal("usage: pclnpredict OLD NEW")
	}
	old, err := loadBin(fs.Arg(0))
	must(err)
	new, err := loadBin(fs.Arg(1))
	must(err)
	tag := "pc-" + baseName(old.Path) + "-" + baseName(new.Path)
	fmt.Printf("== pclnpredict %s -> %s\n", old.Path, new.Path)
	m := matchFuncs(old, new)

	// self-check: findfunctab regeneration
	for _, b := range []*Bin{old, new} {
		got := genFindfunctab(b.Funcs)
		want := findfunctabBytes(b)
		fmt.Printf("findfunctab regeneration for %s: %d bytes, identical=%v\n", baseName(b.Path), len(got), bytes.Equal(got, want))
	}
	// self-check: predicting old from old must be exact
	{
		self := matchFuncs(old, old)
		for _, v := range []pclnVariant{{"self", false, false, false}, {"self-rebuild", true, false, false}, {"self-oracle", true, true, false}, {"self-oracle-names", false, true, true}} {
			p := predictPcln(old, old, self, v, nil, nil)
			d, r := rawDiff(p, old.Pcln.Data)
			fmt.Printf("self-prediction %s: len %d vs %d, differing bytes=%d runs=%d\n", v.name, len(p), len(old.Pcln.Data), d, r)
		}
	}

	// baseline per region
	regions := []struct {
		n string
		f func(p *Pcln) Range
	}{
		{"funcnametab", func(p *Pcln) Range { return p.Funcnametab }},
		{"cutab", func(p *Pcln) Range { return p.Cutab }},
		{"filetab", func(p *Pcln) Range { return p.Filetab }},
		{"pctab", func(p *Pcln) Range { return p.Pctab }},
		{"functab+_func", func(p *Pcln) Range { return p.Functab }},
		{"go:func.*", func(p *Pcln) Range { return p.Gofunc }},
		{"findfunctab", func(p *Pcln) Range { return p.Findfunctab }},
	}
	type rr struct {
		n string
		d Delta
	}
	rows := make([]rr, len(regions))
	var wg sync.WaitGroup
	for i, r := range regions {
		wg.Add(1)
		go func(i int, n string, f func(*Pcln) Range) {
			defer wg.Done()
			var a, b []byte
			switch n {
			case "go:func.*":
				a, b = gofuncBytes(old), gofuncBytes(new)
			case "findfunctab":
				a, b = findfunctabBytes(old), findfunctabBytes(new)
			default:
				a = old.Pcln.Data[f(old.Pcln).Off:f(old.Pcln).End()]
				b = new.Pcln.Data[f(new.Pcln).Off:f(new.Pcln).End()]
			}
			var d Delta
			if bytes.Equal(a, b) {
				d = Delta{OldLen: int64(len(a)), NewLen: int64(len(b))}
			} else {
				d = measure(tag+"-region-"+n, a, b, *which)
			}
			rows[i] = rr{n, d}
		}(i, r.n, r.f)
	}
	var base Delta
	wg.Add(1)
	go func() { defer wg.Done(); base = measure(tag+"-base", old.Pcln.Data, new.Pcln.Data, *which) }()
	variants := []pclnVariant{
		{"V1 functab/entryOff relayout only (names appended, old pctab/funcdata offsets)", false, false, false},
		{"V2 V1 + funcnametab rebuilt in layout order", true, false, false},
		{"V3 V2 + NEW pctab/go:func.* given, old offsets re-based via content map", true, true, false},
		{"V4 NEW funcnametab/cutab/filetab/pctab/go:func.* given (own deltas); nameOff by lookup, other offsets re-based", false, true, true},
	}
	res := make([]Delta, len(variants))
	preds := make([][]byte, len(variants))
	for i, v := range variants {
		wg.Add(1)
		go func(i int, v pclnVariant) {
			defer wg.Done()
			p := predictPcln(old, new, m, v, nil, nil)
			preds[i] = p
			res[i] = measure(fmt.Sprintf("%s-v%d", tag, i), p, new.Pcln.Data, *which)
		}(i, v)
	}
	wg.Wait()
	fmt.Printf("%-16s %10s %10s %10s %10s %10s %10s\n", "region", "newsize", "rawdiff", "runs", "bsdiff", "hdiffz", "zstd")
	for _, r := range rows {
		fmt.Printf("%-16s %10d %10d %10d %10d %10d %10d\n", r.n, r.d.NewLen, r.d.Diff, r.d.Runs, r.d.Bsdiff, r.d.Hdiffz, r.d.Zstd)
	}
	fmt.Printf("BASELINE old.gopclntab -> new.gopclntab: %s\n", base)
	for i, v := range variants {
		fmt.Printf("%s:\n    %s (pred len %d vs new %d)\n", v.name, res[i], len(preds[i]), len(new.Pcln.Data))
	}
	// V3/V4 accounting: the decoder first receives the blob-table deltas
	fnm, cut, fil, pct, gf := rows[0].d, rows[1].d, rows[2].d, rows[3].d, rows[5].d
	fmt.Printf("V3 total = V3 residual + pctab delta + go:func.* delta: bsdiff %d+%d+%d=%d hdiffz %d+%d+%d=%d zstd %d+%d+%d=%d\n",
		res[2].Bsdiff, pct.Bsdiff, gf.Bsdiff, res[2].Bsdiff+pct.Bsdiff+gf.Bsdiff,
		res[2].Hdiffz, pct.Hdiffz, gf.Hdiffz, res[2].Hdiffz+pct.Hdiffz+gf.Hdiffz,
		res[2].Zstd, pct.Zstd, gf.Zstd, res[2].Zstd+pct.Zstd+gf.Zstd)
	sum := func(f func(Delta) int64) int64 { return f(res[3]) + f(fnm) + f(cut) + f(fil) + f(pct) + f(gf) }
	fmt.Printf("V4 total = V4 residual + funcnametab + cutab + filetab + pctab + go:func.* deltas: bsdiff %d+%d+%d+%d+%d+%d=%d hdiffz %d zstd %d\n",
		res[3].Bsdiff, fnm.Bsdiff, cut.Bsdiff, fil.Bsdiff, pct.Bsdiff, gf.Bsdiff, sum(func(d Delta) int64 { return d.Bsdiff }),
		sum(func(d Delta) int64 { return d.Hdiffz }), sum(func(d Delta) int64 { return d.Zstd }))
	// where does the V4 residual live?
	p := preds[3]
	np := new.Pcln
	fmt.Printf("V4 residual by region (differing bytes at same offset, new layout):")
	for _, r := range regions {
		rg := r.f(np)
		if !np.Inside && (r.n == "go:func.*" || r.n == "findfunctab") {
			continue
		}
		if rg.End() > uint64(len(p)) {
			fmt.Printf(" %s=n/a", r.n)
			continue
		}
		d, _ := rawDiff(p[rg.Off:rg.End()], np.Data[rg.Off:rg.End()])
		fmt.Printf(" %s=%d", r.n, d)
	}
	fmt.Println()
	// pctab/gofunc map quality
	if true {
		pcmap := buildDataMapFuzzy(old.Pcln.Data[old.Pcln.Pctab.Off:old.Pcln.Pctab.End()], np.Data[np.Pctab.Off:np.Pctab.End()], gfBlock, nil, gfTol)
		gfmap := buildDataMapFuzzy(gofuncBytes(old), gofuncBytes(new), gfBlock, nil, gfTol)
		fmt.Printf("pctab map: %s; go:func.* map: %s\n", pcmap, gfmap)
	}
}
