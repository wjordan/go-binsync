package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"sort"
	"sync"
)

// predictDataSection re-lays the old section's blocks through the content
// map (nil = identity) and rewrites absolute pointers through the function
// map / data maps. The result has the new section's length.
// dst, if non-nil, receives the prediction in place (it must be zeroed and
// ns.Size long).
func predictDataSection(old, new *Bin, name string, dm *DataMap, mp *mapper, dst []byte) []byte {
	os, ns := old.Sects[name], new.Sects[name]
	olo, ohi := imageRange(old)
	pred := dst
	if pred == nil {
		pred = make([]byte, ns.Size)
	}
	block := 16
	if dm != nil {
		block = dm.Block
	}
	delta := func(o int) int64 {
		if dm == nil || len(dm.Delta) == 0 {
			return 0
		}
		i := o / dm.Block
		if i >= len(dm.Delta) {
			i = len(dm.Delta) - 1
		}
		return dm.Delta[i]
	}
	for o := 0; o < len(os.Data); o += block {
		e := o + block
		if e > len(os.Data) {
			e = len(os.Data)
		}
		p := int64(o) + delta(o)
		if p < 0 || int(p)+(e-o) > len(pred) {
			continue
		}
		copy(pred[p:], os.Data[o:e])
	}
	for o := 0; o+8 <= len(os.Data); o += 8 {
		v := binary.LittleEndian.Uint64(os.Data[o:])
		if v < olo || v >= ohi {
			continue
		}
		p := int64(o) + delta(o)
		if p < 0 || int(p)+8 > len(pred) {
			continue
		}
		nv, cls := mp.mapAddr(v, nil)
		if cls == rcTextUnmatch || cls == rcOutside {
			continue
		}
		binary.LittleEndian.PutUint64(pred[p:], nv)
	}
	return pred
}

// predictWhole builds the predicted new file. new is the decoder's skeleton
// (section table, moduledata values, function list, stage-1 blobs in a fake
// pclntab); nothing else of the new binary is read. The base is a copy of the
// old file (ELF/program/section headers, gaps), then every allocated
// section is overwritten with its prediction.
func predictWhole(old, new *Bin, lay *Layout, m *Match, mp *mapper) (pred []byte, st relocStats) {
	pred, st, _ = predictWholeStats(old, new, lay, m, mp)
	return
}

func predictWholeStats(old, new *Bin, lay *Layout, m *Match, mp *mapper) (pred []byte, st relocStats, ts typeStats) {
	pred = make([]byte, lay.NewLen)
	copy(pred, old.File)
	ptrOnly := map[string]bool{}
	for _, n := range ptrRewriteSects {
		ptrOnly[n] = true
	}
	// every section is predicted independently and in parallel, directly
	// into its (disjoint, zeroed) range of pred where the predictor allows,
	// otherwise into a buffer copied afterwards
	preds := make([][]byte, len(new.Order))
	var wg sync.WaitGroup
	for k, ns := range new.Order {
		if ns.NoBits || ns.Off+ns.Size > uint64(len(pred)) {
			continue
		}
		os := old.Sects[ns.Name]
		dst := pred[ns.Off : ns.Off+ns.Size]
		for i := range dst {
			dst[i] = 0
		}
		wg.Add(1)
		go func(k int, ns *Section, dst []byte) {
			defer wg.Done()
			var p []byte
			switch {
			case ns.Name == ".text":
				_, st, _ = predictText(mp, m, m.NewToOld, "reloc", false, dst)
			case ns.Name == ".gopclntab":
				p = predictPcln(old, new, m, pclnVariant{"V4", false, true, true}, lay, mp.blobs)
			case os == nil:
				// section new in this version: nothing to predict from
			case ns.Name == ".plt":
				// PLT stubs are code: rip-relative jmp/push into .got.plt
				p = relocateBytes(mp, os.Data, &Func{Entry: os.Addr, End: os.Addr + os.Size}, &Func{Entry: ns.Addr, End: ns.Addr + ns.Size}, &relocStats{}, nil)
			case mp.dataMaps[ns.Name] != nil:
				predictDataSection(old, new, ns.Name, mp.dataMaps[ns.Name], mp, dst)
				if tsec := old.sectionOf(old.Mod.Types); typeRewrite && tsec != nil && tsec.Name == ns.Name {
					ts = rewriteTypeOffsets(old, new, ns.Name, mp.dataMaps[ns.Name], mp, dst, nil)
				}
			case ptrOnly[ns.Name]:
				predictDataSection(old, new, ns.Name, nil, mp, dst)
			case ns.Name == ".typelink" && mp.dataMaps[".rodata"] != nil:
				// Go <= 1.26: int32 offsets from runtime.types (inside .rodata)
				ro := old.Sects[".rodata"]
				p = make([]byte, len(os.Data))
				for i := 0; i+4 <= len(os.Data); i += 4 {
					off := int64(binary.LittleEndian.Uint32(os.Data[i:]))
					a := int64(old.Mod.Types) + off - int64(ro.Addr)
					na := int64(mp.dataMaps[".rodata"].Map(uint64(a))) + int64(new.Sects[".rodata"].Addr) - int64(new.Mod.Types)
					binary.LittleEndian.PutUint32(p[i:], uint32(na))
				}
			case ns.Name == ".itablink":
				// Go <= 1.26: absolute pointers to itabs in .rodata
				p = make([]byte, len(os.Data))
				for i := 0; i+8 <= len(os.Data); i += 8 {
					v := binary.LittleEndian.Uint64(os.Data[i:])
					nv, cls := mp.mapAddr(v, nil)
					if cls == rcOutside || cls == rcTextUnmatch {
						nv = v
					}
					binary.LittleEndian.PutUint64(p[i:], nv)
				}
			default:
				p = os.Data
			}
			if p != nil {
				copy(dst, p)
			}
			preds[k] = nil
		}(k, ns, dst)
	}
	wg.Wait()
	return pred, st, ts
}

// deriveOverrides is the encoder-side target vote. Every reference whose
// own new position is known (an absolute pointer in a data section, or a
// nameOff/typeOff/textOff field of a type descriptor, in a block the content
// map matched or placed in a stable run) is read back from the new binary
// at that position: the value there is the reference's true new target.
// The votes are used twice:
//
//  1. blocks the content map did not match (changed embedded offsets, or
//     ambiguous content) whose targets' majority vote implies a shift other
//     than the map's get that shift (transmitted through the RLE map, and
//     propagated forward over the following unmatched blocks); with better
//     positions a second round collects more votes;
//  2. targets the majority still mispredicts get an explicit override (old
//     -> new address) when at least ovMinVotes references agree.
//
// This resolves targets the content map cannot place by content (short,
// repetitive symbols such as runtime.gcbits.* that every window matches at
// several shifts) and the descriptors whose only change is an embedded
// offset.
type overrideStats struct{ Pointers, Fields, Targets, Wrong, BlockFixes, Overrides, Votes int }

func (s overrideStats) String() string {
	return fmt.Sprintf("pointers=%d fields=%d targets=%d mispredicted=%d block-fixes=%d overrides=%d (%d votes)", s.Pointers, s.Fields, s.Targets, s.Wrong, s.BlockFixes, s.Overrides, s.Votes)
}

var (
	ovMinVotes = 2 // references that must agree for a per-target override
	ovRounds   = 2 // vote / block-fix rounds
)

type vote struct{ o, n uint64 }

func collectVotes(old, new *Bin, dmaps map[string]*DataMap, mp *mapper, st *overrideStats) []vote {
	olo, ohi := imageRange(old)
	nlo, nhi := imageRange(new)
	var votes []vote
	sects := append(append([]string{}, dataMapSects...), ptrRewriteSects...)
	for _, name := range sects {
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
			st.Pointers++
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
	// relative fields of the type descriptors
	if tsec := old.sectionOf(old.Mod.Types); typeRewrite && tsec != nil && dmaps[tsec.Name] != nil && new.Sects[tsec.Name] != nil {
		dm := dmaps[tsec.Name]
		nd := new.Sects[tsec.Name].Data
		rewriteTypeOffsets(old, new, tsec.Name, dm, mp, nil, func(off uint64, target uint64, kind byte) {
			st.Fields++
			i := int(off) / dm.Block
			if !dm.reliable(i) {
				return
			}
			p := int64(off) + dm.Delta[i]
			if p < 0 || int(p)+4 > len(nd) {
				return
			}
			v := uint64(binary.LittleEndian.Uint32(nd[p:]))
			var nt uint64
			if kind == 'x' {
				nt = new.Mod.Text + v
			} else {
				nt = new.Mod.Types + v
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

// majorities reduces sorted votes to one (old target, winning new target,
// winning count, total) per old target.
type majority struct {
	o, n         uint64
	count, total int
}

func majorities(votes []vote) []majority {
	var out []majority
	for i := 0; i < len(votes); {
		j := i
		best, bestN := uint64(0), 0
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

func deriveOverrides(old, new *Bin, m *Match, dmaps map[string]*DataMap, shifts map[string]*ShiftTable) ([]AddrOverride, overrideStats) {
	var st overrideStats
	mp := &mapper{src: old, dst: new, srcToDst: m.OldToNew, dataMaps: dmaps, shiftTabs: shifts}
	var maj []majority
	for round := 0; round < ovRounds; round++ {
		st.Pointers, st.Fields = 0, 0
		maj = majorities(collectVotes(old, new, dmaps, mp, &st))
		// block fixes: per unmatched target block, the majority implied shift
		type bk struct {
			name string
			i    int
		}
		bvotes := map[bk]map[int64]int{}
		for _, mj := range maj {
			if 2*mj.count <= mj.total {
				continue
			}
			s := old.sectionOf(mj.o)
			if s == nil {
				continue
			}
			dm := dmaps[s.Name]
			ns := new.Sects[s.Name]
			if dm == nil || ns == nil {
				continue
			}
			i := int(mj.o-s.Addr) / dm.Block
			if i >= len(dm.Delta) || dm.Matched[i] {
				continue
			}
			d := int64(mj.n-ns.Addr) - int64(mj.o-s.Addr)
			k := bk{s.Name, i}
			if bvotes[k] == nil {
				bvotes[k] = map[int64]int{}
			}
			bvotes[k][d] += mj.count
		}
		fixed := map[string][]bool{}
		for k, vs := range bvotes {
			dm := dmaps[k.name]
			best, bestN, total := int64(0), 0, 0
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
			st.BlockFixes++
		}
		// forward-fill: plain unmatched blocks follow the nearest resolved
		// block before them (as the forward pass did with the previous shift)
		for name, fx := range fixed {
			dm := dmaps[name]
			cur := int64(0)
			for i := range dm.Delta {
				switch {
				case dm.Matched[i] || fx[i]:
					cur = dm.Delta[i]
				case dm.Ambiguous[i]:
					// keep the backward-pass decision
				default:
					dm.Delta[i] = cur
				}
			}
		}
	}
	// per-target overrides against the (fixed) maps
	var out []AddrOverride
	for _, mj := range maj {
		st.Targets++
		pred, cls := mp.mapAddr(mj.o, nil)
		if cls == rcOutside || cls == rcTextUnmatch || mj.n == pred {
			continue
		}
		st.Wrong++
		if mj.count >= ovMinVotes && 2*mj.count > mj.total {
			out = append(out, AddrOverride{mj.o, mj.n})
			st.Overrides++
			st.Votes += mj.count
		}
	}
	return out, st
}

// buildMaps is the encoder side of the content maps and shift tables.
func buildMaps(old, new *Bin, m *Match, block int, lap func(string)) (map[string]*DataMap, map[string]*ShiftTable) {
	if lap == nil {
		lap = func(string) {}
	}
	dmaps := map[string]*DataMap{}
	for _, n := range dataMapSects {
		if old.Sects[n] != nil && new.Sects[n] != nil && !old.Sects[n].NoBits {
			dmaps[n], _, _ = buildDataMapMasked(old, new, n, block)
			lap("content map " + n)
		}
	}
	shifts := deriveShiftTables(old, new, m, []string{".bss", ".noptrbss"})
	lap("shift tables")
	return dmaps, shifts
}

// cmdWhole runs the full predict-then-correct pipeline through the
// transmitted layout (the same path the decoder takes) and measures the
// patch: layout + stage 1 (deltas of the pclntab blob tables) + stage 2
// (one correction over the whole predicted file: positional and generic).
func cmdWhole(args []string) {
	fs := flag.NewFlagSet("whole", flag.ExitOnError)
	which := fs.String("enc", "bhz", "encoders")
	block := fs.Int("block", 16, "data-map block size")
	fs.IntVar(&dataTol, "datatol", 0, "fuzzy tolerance for data-section maps")
	fs.Parse(args)
	if fs.NArg() < 2 {
		fatal("usage: whole OLD NEW")
	}
	old, err := loadBin(fs.Arg(0))
	must(err)
	new, err := loadBin(fs.Arg(1))
	must(err)
	tag := "wh-" + baseName(old.Path) + "-" + baseName(new.Path)
	fmt.Printf("== whole %s -> %s (layout %s -> %s)\n", old.Path, new.Path, old.Layout, new.Layout)
	m := matchFuncs(old, new)
	dmaps, shifts := buildMaps(old, new, m, *block, nil)
	overrides, ost := deriveOverrides(old, new, m, dmaps, shifts)
	fmt.Printf("overrides: %s\n", ost)
	lay := buildLayout(old, new, m, dmaps, shifts, overrides)
	must(layoutRoundTrip(lay, old))
	layRaw := lay.Encode(old)
	layZ := zstdSize(tag+"-layout", layRaw)
	fmt.Printf("match: exact=%d normalised=%d content=%d unmatched-new=%d unmatched-old=%d\n", m.Exact, m.Norm, m.Content, m.Unmatched, m.UnmatchedOld)
	for _, n := range dataMapSects {
		if dm := dmaps[n]; dm != nil {
			fmt.Printf("datamap %s: %s; rle %d B\n", n, dm, len(dm.EncodeRLE()))
		}
	}
	for _, n := range []string{".bss", ".noptrbss"} {
		if st := shifts[n]; st != nil {
			fmt.Printf("shift table %s: %s\n", n, st)
		}
	}
	fmt.Printf("layout (sections + moduledata + %d funcs + %d rec shapes + shift tables + content maps): raw %d B, zstd %d B\n", lay.NFunc, len(lay.RecShapes), len(layRaw), layZ)

	lay2, err := DecodeLayout(layRaw, old)
	must(err)
	skel, m2, err := skeletonBin(old, lay2)
	must(err)
	s1aOld, s1aNew := stage1aBlobs(old), stage1aBlobs(new)
	must(fillSkeleton(skel, skel.Pcln.ranges1a(), s1aNew))
	mp := &mapper{src: old, dst: skel, srcToDst: m2.OldToNew, dataMaps: lay2.DataMaps, shiftTabs: lay2.Shifts, overrides: overrideMap(lay2.Overrides)}
	bp := predictBlobs(old, skel, m2, mp)
	s1bPred, s1bNew := bp.concat(), stage1bBlobs(new)
	must(fillSkeleton(skel, skel.Pcln.ranges1b(), s1bNew))
	mp.blobs = bp
	fmt.Printf("stage-1b prediction: %s\n", bp.Stats)
	pred, st := predictWhole(old, skel, lay2, m2, mp)
	posRaw := encodePositional(pred, new.File)
	posZ := zstdSize(tag+"-pos", posRaw)
	var s1a, s1b, s2, base Delta
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); s1a = measure(tag+"-stage1a", s1aOld, s1aNew, *which) }()
	go func() { defer wg.Done(); s1b = measure(tag+"-stage1b", s1bPred, s1bNew, *which) }()
	go func() { defer wg.Done(); s2 = measure(tag+"-stage2", pred, new.File, *which) }()
	go func() { defer wg.Done(); base = measure(tag+"-base", old.File, new.File, *which) }()
	wg.Wait()
	fmt.Printf("text relocation: insns=%d decode-failures=%d\n", st.Insns, st.Fails)
	fmt.Printf("predicted file: %d B (new %d B), differing bytes=%d runs=%d\n", len(pred), len(new.File), s2.Diff, s2.Runs)
	fmt.Printf("BASELINE whole file:        %s\n", base)
	s1 := Delta{Bsdiff: s1a.Bsdiff + s1b.Bsdiff, Hdiffz: s1a.Hdiffz + s1b.Hdiffz, Zstd: s1a.Zstd + s1b.Zstd}
	fmt.Printf("stage 1a (funcnametab+filetab %d B): %s\n", len(s1aNew), s1a)
	fmt.Printf("stage 1b (cutab+pctab+go:func.* %d B, predicted): %s\n", len(s1bNew), s1b)
	fmt.Printf("stage 2 (predicted file):   %s; positional raw %d B zstd %d B\n", s2, len(posRaw), posZ)
	fmt.Printf("TOTAL Go-aware = layout %d + stage1 + stage2: bsdiff %d (%.1fx smaller) hdiffz %d (%.1fx) zstd %d (%.1fx) positional(hdiffz s1) %d (%.1fx vs bsdiff, %.1fx vs hdiffz)\n",
		layZ, layZ+s1.Bsdiff+s2.Bsdiff, float64(base.Bsdiff)/float64(layZ+s1.Bsdiff+s2.Bsdiff),
		layZ+s1.Hdiffz+s2.Hdiffz, float64(base.Hdiffz)/float64(layZ+s1.Hdiffz+s2.Hdiffz),
		layZ+s1.Zstd+s2.Zstd, float64(base.Zstd)/float64(layZ+s1.Zstd+s2.Zstd),
		layZ+s1.Hdiffz+posZ, float64(base.Bsdiff)/float64(layZ+s1.Hdiffz+posZ), float64(base.Hdiffz)/float64(layZ+s1.Hdiffz+posZ))
	printResidual(pred, new)
}

// printResidual prints where the stage-2 residual lives (differing bytes per section).
func printResidual(pred []byte, new *Bin) {
	fmt.Printf("stage-2 differing bytes by section:")
	for _, ns := range new.Order {
		if ns.NoBits || ns.Off+ns.Size > uint64(len(pred)) {
			continue
		}
		d, r := rawDiff(pred[ns.Off:ns.Off+ns.Size], ns.Data)
		if d > 0 {
			fmt.Printf(" %s=%d(%d runs)", ns.Name, d, r)
		}
	}
	fmt.Println()
}
