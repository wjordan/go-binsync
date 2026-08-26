package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/arch/x86/x86asm"
)

// refClass classifies the target of a PC-relative reference.
type refClass int

const (
	rcTextSelf    refClass = iota // inside the same function
	rcTextMatched                 // another function that exists in new
	rcTextUnmatch                 // an old function with no counterpart
	rcTextNone                    // in .text but not inside any function
	rcRodata
	rcNoptrdata
	rcData
	rcBss
	rcPlt
	rcOtherSect
	rcOutside
	rcNoFit // new displacement does not fit the field
	rcNum
)

var refClassNames = [...]string{"text-self", "text-matched", "text-unmatched", "text-nofunc", "rodata", "noptrdata", "data", "bss/noptrbss", "plt", "other-section", "outside-image", "no-fit"}

type ref struct {
	off   int // offset of the displacement field within the function
	n     int // field length
	class refClass
}

type relocStats struct {
	Insns, Fails int
	Refs         [rcNum]int
}

// mapper maps an absolute address of src into an absolute address of dst.
type mapper struct {
	src, dst  *Bin
	srcToDst  []int                  // function index map
	dataMaps  map[string]*DataMap    // by section name; nil entries => identity
	shiftTabs map[string]*ShiftTable // transmitted piecewise shifts (bss)
	overrides map[uint64]uint64      // transmitted target overrides (old -> new address)
	blobs     *blobPred              // stage-1b prediction (old -> predicted table offsets)
}

func (mp *mapper) mapAddr(t uint64, self *Func) (uint64, refClass) {
	a, cls := mp.mapAddrBase(t, self)
	if mp.overrides != nil {
		if nv, ok := mp.overrides[t]; ok {
			return nv, cls
		}
	}
	return a, cls
}

// mapAddrBase is mapAddr through the function map, content maps and shift
// tables only (no overrides).
func (mp *mapper) mapAddrBase(t uint64, self *Func) (uint64, refClass) {
	src, dst := mp.src, mp.dst
	if t >= src.Text.Addr && t < src.Text.Addr+src.Text.Size {
		f := src.funcAt(t)
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
	s := src.sectionOf(t)
	if s == nil && t > 0 {
		s = src.sectionOf(t - 1) // one-past-the-end references
	}
	if s == nil {
		return 0, rcOutside
	}
	ds := dst.Sects[s.Name]
	if ds == nil {
		return 0, rcOutside
	}
	off := t - s.Addr
	var cls refClass
	switch s.Name {
	case ".rodata":
		cls = rcRodata
	case ".noptrdata":
		cls = rcNoptrdata
	case ".data":
		cls = rcData
	case ".bss", ".noptrbss":
		cls = rcBss
	case ".plt":
		cls = rcPlt
	default:
		cls = rcOtherSect
	}
	if dm := mp.dataMaps[s.Name]; dm != nil {
		off = dm.Map(off)
	} else if st := mp.shiftTabs[s.Name]; st != nil {
		off = st.Map(off)
	}
	return ds.Addr + off, cls
}

// relocate decodes the bytes of srcFn (from src) and re-encodes every
// PC-relative displacement for the function's new position dstFn (in dst).
// The result has exactly dstFn.Size() bytes (truncated or padded with INT3).
func relocate(mp *mapper, srcFn, dstFn *Func, st *relocStats, refs *[]ref) []byte {
	return relocateBytes(mp, mp.src.funcBytes(srcFn), srcFn, dstFn, st, refs)
}

// relocateBytes is relocate over an explicit byte range (used for .plt,
// where srcFn/dstFn are synthetic ranges).
func relocateBytes(mp *mapper, code []byte, srcFn, dstFn *Func, st *relocStats, refs *[]ref) []byte {
	out := make([]byte, dstFn.Size())
	for i := range out {
		out[i] = 0xCC
	}
	n := copy(out, code)
	code = code[:n]
	for i := 0; i < len(code); {
		inst, err := x86asm.Decode(code[i:], 64)
		if err != nil || inst.Len == 0 {
			st.Fails++
			i++
			continue
		}
		st.Insns++
		if inst.PCRel > 0 && i+inst.PCRelOff+inst.PCRel <= len(code) {
			fld := code[i+inst.PCRelOff : i+inst.PCRelOff+inst.PCRel]
			var disp int64
			switch inst.PCRel {
			case 1:
				disp = int64(int8(fld[0]))
			case 2:
				disp = int64(int16(binary.LittleEndian.Uint16(fld)))
			case 4:
				disp = int64(int32(binary.LittleEndian.Uint32(fld)))
			}
			oldPC := srcFn.Entry + uint64(i)
			newPC := dstFn.Entry + uint64(i)
			target := uint64(int64(oldPC) + int64(inst.Len) + disp)
			nt, cls := mp.mapAddr(target, srcFn)
			if cls == rcTextUnmatch || cls == rcOutside {
				// leave the displacement unchanged
			} else {
				nd := int64(nt) - int64(newPC) - int64(inst.Len)
				fits := true
				switch inst.PCRel {
				case 1:
					fits = nd >= -128 && nd <= 127
					if fits {
						out[i+inst.PCRelOff] = byte(int8(nd))
					}
				case 2:
					fits = nd >= -32768 && nd <= 32767
					if fits {
						binary.LittleEndian.PutUint16(out[i+inst.PCRelOff:], uint16(int16(nd)))
					}
				case 4:
					fits = nd >= -1<<31 && nd < 1<<31
					if fits {
						binary.LittleEndian.PutUint32(out[i+inst.PCRelOff:], uint32(int32(nd)))
					}
				}
				if !fits {
					cls = rcNoFit
				}
			}
			st.Refs[cls]++
			if refs != nil {
				*refs = append(*refs, ref{i + inst.PCRelOff, inst.PCRel, cls})
			}
		}
		i += inst.Len
	}
	return out
}

// predictText builds the predicted .text of dst from src (into out if
// non-nil, which must be dst.Text.Size long).
// mode: "copy" (no relocation), "reloc".
func predictText(mp *mapper, m *Match, dstToSrc []int, mode string, wantRefs bool, out []byte) (pred []byte, st relocStats, fnRefs [][]ref) {
	src, dst := mp.src, mp.dst
	pred = out
	if pred == nil {
		pred = make([]byte, dst.Text.Size)
	}
	for i := range pred {
		pred[i] = 0xCC
	}
	// prefix before the first function and suffix after the last: copy raw
	copy(pred, src.Text.Data[:src.Funcs[0].Entry-src.Text.Addr])
	sufOld := src.Text.Data[src.Funcs[len(src.Funcs)-1].End-src.Text.Addr:]
	sufNew := pred[dst.Funcs[len(dst.Funcs)-1].End-dst.Text.Addr:]
	copy(sufNew, sufOld)
	if wantRefs {
		fnRefs = make([][]ref, len(dst.Funcs))
	}
	stats := make([]relocStats, len(dst.Funcs))
	var wg sync.WaitGroup
	work := make(chan int, 1024)
	for w := 0; w < 24; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				g := dst.Funcs[j]
				i := dstToSrc[j]
				if i < 0 {
					continue // new function: left as INT3
				}
				f := src.Funcs[i]
				var b []byte
				if mode == "copy" {
					b = make([]byte, g.Size())
					for k := range b {
						b[k] = 0xCC
					}
					copy(b, src.funcBytes(f))
				} else {
					var refs *[]ref
					if wantRefs {
						refs = &fnRefs[j]
					}
					b = relocate(mp, f, g, &stats[j], refs)
				}
				copy(pred[g.Entry-dst.Text.Addr:], b)
			}
		}()
	}
	for j := range dst.Funcs {
		work <- j
	}
	close(work)
	wg.Wait()
	for _, s := range stats {
		st.Insns += s.Insns
		st.Fails += s.Fails
		for c := range s.Refs {
			st.Refs[c] += s.Refs[c]
		}
	}
	return
}

func cmdPredict(args []string) {
	fs := flag.NewFlagSet("predict", flag.ExitOnError)
	block := fs.Int("block", 16, "data-map block size")
	fs.IntVar(&dataTol, "datatol", 0, "fuzzy tolerance for data-section maps")
	mask := fs.Bool("mask", true, "mask absolute pointers when building data maps")
	which := fs.String("enc", "bhz", "encoders to run: b=bsdiff h=hdiffz z=zstd")
	dump := fs.Int("dump", 0, "dump up to N mispredicted references")
	fs.Parse(args)
	if fs.NArg() < 2 {
		fatal("usage: predict [-block N] OLD NEW")
	}
	old, err := loadBin(fs.Arg(0))
	must(err)
	new, err := loadBin(fs.Arg(1))
	must(err)
	tag := "pt-" + baseName(old.Path) + "-" + baseName(new.Path)
	fmt.Printf("== predict .text %s -> %s\n", old.Path, new.Path)
	m := matchFuncs(old, new)
	fmt.Printf("match: exact=%d normalised=%d content=%d unmatched-new=%d unmatched-old=%d\n", m.Exact, m.Norm, m.Content, m.Unmatched, m.UnmatchedOld)
	lay := encodeLayout(old, new, m)
	fmt.Printf("layout table: raw %d B, zstd %d B\n", len(lay), zstdSize(tag+"-layout", lay))

	// decode throughput
	t0 := time.Now()
	var insns, fails int
	for _, f := range old.Funcs {
		code := old.funcBytes(f)
		for i := 0; i < len(code); {
			inst, err := x86asm.Decode(code[i:], 64)
			if err != nil || inst.Len == 0 {
				fails++
				i++
				continue
			}
			insns++
			i += inst.Len
		}
	}
	dt := time.Since(t0)
	fmt.Printf("x86asm decode of old .text: %d insns, %d failures, %.3fs => %.1f MB/s single-thread\n", insns, fails, dt.Seconds(), float64(old.Text.Size)/1e6/dt.Seconds())

	// data maps
	t0 = time.Now()
	dmaps := map[string]*DataMap{}
	for _, name := range []string{".rodata", ".noptrdata", ".data"} {
		os, ns := old.Sects[name], new.Sects[name]
		if os == nil || ns == nil {
			continue
		}
		var dm *DataMap
		np := 0
		if *mask {
			dm, np, _ = buildDataMapMasked(old, new, name, *block)
		} else {
			dm = buildDataMap(os.Data, ns.Data, *block)
		}
		dmaps[name] = dm
		fmt.Printf("datamap %s (block %d, masked pointers=%d): %s\n", name, *block, np, dm)
	}
	fmt.Printf("datamap build time: %.3fs\n", time.Since(t0).Seconds())

	// bss shift tables (derived by the encoder from unchanged functions, transmitted)
	shifts := deriveShiftTables(old, new, m, []string{".bss", ".noptrbss"})
	shiftBytes := 0
	for _, n := range []string{".bss", ".noptrbss"} {
		if st := shifts[n]; st != nil {
			fmt.Printf("shift table %s: %s\n", n, st)
			shiftBytes += len(st.Encode())
		}
	}
	fmt.Printf("shift tables raw total: %d B\n", shiftBytes)

	type variant struct {
		name   string
		mode   string
		maps   map[string]*DataMap
		shifts map[string]*ShiftTable
	}
	variants := []variant{
		{"P0 layout-copy (no relocation)", "copy", nil, nil},
		{"P1 reloc, M=section-identity", "reloc", nil, nil},
		{"P2 reloc, M=content-map+bss-shift", "reloc", dmaps, shifts},
	}
	base := measure(tag+"-base", old.Text.Data, new.Text.Data, *which)
	fmt.Printf("BASELINE old.text -> new.text: %s (bsdiff %.1fs hdiffz %.1fs zstd %.1fs)\n", base, base.TBsdiff.Seconds(), base.THdiffz.Seconds(), base.TZstd.Seconds())
	type result struct {
		v    variant
		d    Delta
		st   relocStats
		pred []byte
		refs [][]ref
		t    time.Duration
	}
	results := make([]result, len(variants))
	var wg sync.WaitGroup
	for vi, v := range variants {
		wg.Add(1)
		go func(vi int, v variant) {
			defer wg.Done()
			mp := &mapper{src: old, dst: new, srcToDst: m.OldToNew, dataMaps: v.maps, shiftTabs: v.shifts}
			t := time.Now()
			pred, st, refs := predictText(mp, m, m.NewToOld, v.mode, true, nil)
			el := time.Since(t)
			d := measure(fmt.Sprintf("%s-v%d", tag, vi), pred, new.Text.Data, *which)
			results[vi] = result{v, d, st, pred, refs, el}
		}(vi, v)
	}
	wg.Wait()
	for _, r := range results {
		fmt.Printf("%-34s %s  predict-time=%.2fs\n", r.v.name, r.d, r.t.Seconds())
		if r.v.mode == "reloc" {
			fmt.Printf("    insns=%d decode-failures=%d refs:", r.st.Insns, r.st.Fails)
			for c := 0; c < int(rcNum); c++ {
				if r.st.Refs[c] > 0 {
					fmt.Printf(" %s=%d", refClassNames[c], r.st.Refs[c])
				}
			}
			fmt.Println()
		}
	}

	// breakdown of the remaining differing bytes for the best variant (P2)
	best := results[len(results)-1]
	breakdown(old, new, m, best.pred, best.refs)
	if *dump > 0 {
		dumpMispredictions(old, new, m, best.pred, best.refs, dmaps, *dump)
	}

	// changed-function lower bound
	var chg []int
	var chgOld, chgNew []byte
	var sizeOld, sizeNew uint64
	for j, g := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			chg = append(chg, j)
			chgNew = append(chgNew, new.funcBytes(g)...)
			sizeNew += g.Size()
			continue
		}
		f := old.Funcs[i]
		if contentHash(old.funcBytes(f)) != contentHash(new.funcBytes(g)) {
			chg = append(chg, j)
			chgOld = append(chgOld, old.funcBytes(f)...)
			chgNew = append(chgNew, new.funcBytes(g)...)
			sizeOld += f.Size()
			sizeNew += g.Size()
		}
	}
	names := []string{}
	for _, j := range chg {
		if len(names) < 20 {
			names = append(names, new.Funcs[j].Name)
		}
	}
	lb := measure(tag+"-changedfns", chgOld, chgNew, *which)
	fmt.Printf("changed functions (content differs after masking PC-rel fields, or new): %d, old %d B -> new %d B; delta of just those: %s; new bytes alone zstd=%d\n", len(chg), sizeOld, sizeNew, lb, zstdSize(tag+"-chgnew", chgNew))
	fmt.Printf("  changed: %s\n", strings.Join(names, ", "))

	// invertibility: relocate new -> old layout and compare with old
	{
		rmaps := map[string]*DataMap{}
		for _, name := range []string{".rodata", ".noptrdata", ".data"} {
			os, ns := old.Sects[name], new.Sects[name]
			if os == nil || ns == nil {
				continue
			}
			if *mask {
				rmaps[name], _, _ = buildDataMapMasked(new, old, name, *block)
			} else {
				rmaps[name] = buildDataMap(ns.Data, os.Data, *block)
			}
		}
		rshifts := deriveShiftTables(new, old, &Match{NewToOld: m.OldToNew, OldToNew: m.NewToOld}, []string{".bss", ".noptrbss"})
		mp := &mapper{src: new, dst: old, srcToDst: m.NewToOld, dataMaps: rmaps, shiftTabs: rshifts}
		predOld, _, _ := predictText(mp, m, m.OldToNew, "reloc", false, nil)
		mism, mismChanged := 0, 0
		var ex []string
		for i, f := range old.Funcs {
			j := m.OldToNew[i]
			if j < 0 {
				continue
			}
			a := old.funcBytes(f)
			b := predOld[f.Entry-old.Text.Addr : f.End-old.Text.Addr]
			if !bytes.Equal(a, b) {
				mism++
				if contentHash(a) != contentHash(new.funcBytes(new.Funcs[j])) {
					mismChanged++
				} else if len(ex) < 10 {
					ex = append(ex, f.Name)
				}
			}
		}
		d := measure(tag+"-inverse", predOld, old.Text.Data, *which)
		fmt.Printf("invertibility: relocating NEW->OLD layout: %d matched functions differ from OLD, of which %d have changed content; unchanged-content mismatches: %v; residual %s\n", mism, mismChanged, ex, d)
	}
}

// breakdown attributes the bytes where pred != new to causes.
func breakdown(old, new *Bin, m *Match, pred []byte, refs [][]ref) {
	var byClass [rcNum]int
	var changedFn, newFn, nonRef, decodeAdj int
	var fnDiff []struct {
		name string
		n    int
	}
	total := 0
	for j, g := range new.Funcs {
		a := pred[g.Entry-new.Text.Addr : g.End-new.Text.Addr]
		b := new.funcBytes(g)
		if bytes.Equal(a, b) {
			continue
		}
		i := m.NewToOld[j]
		nd := 0
		for k := range a {
			if a[k] != b[k] {
				nd++
			}
		}
		total += nd
		if i < 0 {
			newFn += nd
			continue
		}
		if contentHash(old.funcBytes(old.Funcs[i])) != contentHash(b) {
			changedFn += nd
			fnDiff = append(fnDiff, struct {
				name string
				n    int
			}{g.Name, nd})
			continue
		}
		// unchanged content but mispredicted references
		isRef := make([]refClass, len(a))
		for k := range isRef {
			isRef[k] = -1
		}
		for _, r := range refs[j] {
			for k := r.off; k < r.off+r.n && k < len(a); k++ {
				isRef[k] = r.class
			}
		}
		for k := range a {
			if a[k] != b[k] {
				if isRef[k] >= 0 {
					byClass[isRef[k]]++
				} else {
					nonRef++
				}
			}
		}
		fnDiff = append(fnDiff, struct {
			name string
			n    int
		}{g.Name, nd})
	}
	// bytes outside functions
	outside := 0
	{
		pre := int(new.Funcs[0].Entry - new.Text.Addr)
		for k := 0; k < pre; k++ {
			if pred[k] != new.Text.Data[k] {
				outside++
			}
		}
		for k := int(new.Funcs[len(new.Funcs)-1].End - new.Text.Addr); k < len(pred); k++ {
			if pred[k] != new.Text.Data[k] {
				outside++
			}
		}
	}
	_ = decodeAdj
	fmt.Printf("residual breakdown (P2): total differing bytes in functions=%d: changed-function content=%d, new functions=%d, mispredicted refs:", total, changedFn, newFn)
	for c := 0; c < int(rcNum); c++ {
		if byClass[c] > 0 {
			fmt.Printf(" %s=%d", refClassNames[c], byClass[c])
		}
	}
	fmt.Printf(", non-reference bytes in unchanged functions=%d, outside functions=%d\n", nonRef, outside)
	sort.Slice(fnDiff, func(i, j int) bool { return fnDiff[i].n > fnDiff[j].n })
	fmt.Printf("functions with residual differences: %d; top:", len(fnDiff))
	for i := 0; i < len(fnDiff) && i < 15; i++ {
		fmt.Printf(" %s=%d", fnDiff[i].name, fnDiff[i].n)
	}
	fmt.Println()
}

// dumpMispredictions prints mispredicted references of unchanged functions:
// old target, predicted new target, actual new target (decoded from the same
// instruction in the new binary).
func dumpMispredictions(old, new *Bin, m *Match, pred []byte, refs [][]ref, dmaps map[string]*DataMap, limit int) {
	hist := map[int64]int{}
	shown := 0
	for j, g := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		a := pred[g.Entry-new.Text.Addr : g.End-new.Text.Addr]
		b := new.funcBytes(g)
		if bytes.Equal(a, b) {
			continue
		}
		f := old.Funcs[i]
		if contentHash(old.funcBytes(f)) != contentHash(b) {
			continue
		}
		oc := old.funcBytes(f)
		for _, r := range refs[j] {
			same := true
			for k := r.off; k < r.off+r.n; k++ {
				if a[k] != b[k] {
					same = false
				}
			}
			if same || r.n != 4 {
				continue
			}
			// instruction start: find by decoding forward
			insnStart := -1
			for k := 0; k < len(oc); {
				inst, err := x86asm.Decode(oc[k:], 64)
				if err != nil || inst.Len == 0 {
					k++
					continue
				}
				if k+inst.PCRelOff == r.off {
					insnStart = k
					// next-pc based targets
					oldT := int64(f.Entry) + int64(k+inst.Len) + int64(int32(binary.LittleEndian.Uint32(oc[r.off:])))
					predT := int64(g.Entry) + int64(k+inst.Len) + int64(int32(binary.LittleEndian.Uint32(a[r.off:])))
					actT := int64(g.Entry) + int64(k+inst.Len) + int64(int32(binary.LittleEndian.Uint32(b[r.off:])))
					hist[actT-predT]++
					if shown < limit {
						os := old.sectionOf(uint64(oldT))
						sn, so := "?", int64(0)
						if os != nil {
							sn, so = os.Name, oldT-int64(os.Addr)
						}
						ns := new.sectionOf(uint64(actT))
						nso := int64(0)
						if ns != nil {
							nso = actT - int64(ns.Addr)
						}
						blk := ""
						if dm := dmaps[sn]; dm != nil && so >= 0 {
							bi := int(so) / dm.Block
							if bi < len(dm.Delta) {
								blk = fmt.Sprintf(" block=%d delta=%d matched=%v", bi, dm.Delta[bi], dm.Matched[bi])
							}
						}
						fmt.Printf("  mispred %s+%#x %s: old %s+%d -> pred %+d actual %s+%d (act-pred=%+d)%s\n", g.Name, k, inst.Op, sn, so, predT-int64(new.Sects[sn].Addr)-so, ns.Name, nso, actT-predT, blk)
						shown++
					}
					break
				}
				k += inst.Len
			}
			_ = insnStart
		}
	}
	fmt.Printf("  mispred histogram (actual-predicted): %v\n", hist)
}
