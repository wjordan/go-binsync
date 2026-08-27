package delta

import (
	"fmt"
	"sort"

	"github.com/wjordan/go-binsync/delta/gobin"
)

// layout is everything the decoder needs besides the old binary and the
// stage-1 tables: the new file's length and section table, the moduledata
// values the predictors read, the function order and sizes, the pclntab
// table offsets, the data maps and the shift tables. It is transmitted, so
// every field here costs patch bytes and every field is a delta against the
// old binary's corresponding value.
//
// Its real job is to make the prediction *length-exact*. Once the layout is
// applied, the predicted file has precisely the new file's length and every
// function, table and section sits at precisely its final offset -- even
// where the predicted content is wrong. That is what lets the correction be
// positional and lets the encoder skip the whole-file suffix array.
type layout struct {
	NewLen uint64
	Sects  []sectInfo

	// moduledata values the predictors read
	Text, Types, Etypes, Typedesclen, Itaboffset, Itabsize uint64
	Rodata, Gofunc, Findfunctab, Epclntab, Minpc, Maxpc    uint64

	FirstEntry uint64
	NFunc      int
	Funcs      []byte // the op stream below

	NFiles  uint64
	PclnLen uint64
	// starts of funcnametab, cutab, filetab, pctab, functab, go:func.* and
	// findfunctab; each table runs to the next, so the stage-1 blob lengths
	// follow from these and the prediction can be padded to be length-exact
	TabOff [7]uint64

	// (npcdata, nfuncdata) for the records whose shape the decoder would
	// otherwise get wrong
	RecShapes []recShape

	Shifts    map[string]*shiftTable
	DataMaps  map[string]*dataMap
	Overrides []addrOverride
}

type sectInfo struct {
	Name            string
	Addr, Off, Size uint64
	NoBits          bool
}

type recShape struct {
	Idx                int
	Npcdata, Nfuncdata uint32
}

// addrOverride pins one old address to its true new address, for targets the
// content map places wrongly.
type addrOverride struct{ Old, New uint64 }

const layoutMagic = "BSL1"

// maxFuncs bounds what a patch may claim, so a corrupt layout cannot become
// an allocation.
const maxFuncs = 1 << 24

// encodeSects writes the new section table as deltas against the old one.
func encodeSects(w *wbuf, old *gobin.Bin, sects []sectInfo) {
	w.u(uint64(len(sects)))
	expect := 0
	for _, s := range sects {
		k := -1
		for i, os := range old.Order {
			if os.Name == s.Name {
				k = i
				break
			}
		}
		var oa, oo, osz uint64
		switch {
		case k < 0:
			w.raw([]byte{2})
			w.str(s.Name)
		case k == expect:
			w.raw([]byte{0})
			expect++
		default:
			w.raw([]byte{1})
			w.u(uint64(k))
			expect = k + 1
		}
		if k >= 0 {
			oa, oo, osz = old.Order[k].Addr, old.Order[k].Off, old.Order[k].Size
		}
		w.s(int64(s.Addr) - int64(oa))
		w.s(int64(s.Off) - int64(oo))
		w.s(int64(s.Size) - int64(osz))
		nb := byte(0)
		if s.NoBits {
			nb = 1
		}
		w.raw([]byte{nb})
	}
}

func decodeSects(r *rbuf, old *gobin.Bin) []sectInfo {
	n := r.un(1<<12, "section count")
	out := make([]sectInfo, 0, n)
	expect := 0
	for i := uint64(0); i < n && r.err == nil; i++ {
		var s sectInfo
		k := -1
		switch op := r.byte(); op {
		case 0:
			k = expect
		case 1:
			k = int(r.un(uint64(len(old.Order)), "old section index"))
		case 2:
			s.Name = string(r.take(r.un(256, "section name length")))
		default:
			r.fail("bad section op %d", op)
			return nil
		}
		var oa, oo, osz uint64
		if k >= 0 {
			if k >= len(old.Order) {
				r.fail("old section index %d out of range", k)
				return nil
			}
			s.Name = old.Order[k].Name
			oa, oo, osz = old.Order[k].Addr, old.Order[k].Off, old.Order[k].Size
			expect = k + 1
		}
		s.Addr = uint64(int64(oa) + r.s())
		s.Off = uint64(int64(oo) + r.s())
		s.Size = uint64(int64(osz) + r.s())
		s.NoBits = r.byte() == 1
		out = append(out, s)
	}
	return out
}

// modValues pairs the layout's moduledata fields with the old binary's, in
// the order both sides serialise them.
func (l *layout) modValues(om *gobin.Moduledata) [][2]*uint64 {
	return [][2]*uint64{
		{&l.Text, &om.Text}, {&l.Types, &om.Types}, {&l.Etypes, &om.Etypes},
		{&l.Typedesclen, &om.Typedesclen}, {&l.Itaboffset, &om.Itaboffset}, {&l.Itabsize, &om.Itabsize},
		{&l.Rodata, &om.Rodata}, {&l.Gofunc, &om.Gofunc}, {&l.Findfunctab, &om.Findfunctab},
		{&l.Epclntab, &om.Epclntab}, {&l.Minpc, &om.Minpc}, {&l.Maxpc, &om.Maxpc},
	}
}

func oldTabOffs(p *gobin.Pcln) [7]uint64 {
	return [7]uint64{p.Funcnametab.Off, p.Cutab.Off, p.Filetab.Off, p.Pctab.Off, p.Functab.Off, p.Gofunc.Off, p.Findfunctab.Off}
}

func (l *layout) encode(old *gobin.Bin) []byte {
	w := &wbuf{}
	w.raw([]byte(layoutMagic))
	w.s(int64(l.NewLen) - int64(len(old.File)))
	encodeSects(w, old, l.Sects)
	for _, v := range l.modValues(old.Mod) {
		w.s(int64(*v[0]) - int64(*v[1]))
	}
	w.s(int64(l.FirstEntry) - int64(old.Funcs[0].Entry))
	w.u(uint64(l.NFunc))
	w.bytes(l.Funcs)
	w.s(int64(l.NFiles) - int64(old.Pcln.NFiles))
	w.s(int64(l.PclnLen) - int64(len(old.Pcln.Data)))
	ot := oldTabOffs(old.Pcln)
	for i, v := range l.TabOff {
		w.s(int64(v) - int64(ot[i]))
	}
	w.u(uint64(len(l.RecShapes)))
	prev := 0
	for _, r := range l.RecShapes {
		w.u(uint64(r.Idx - prev))
		w.u(uint64(r.Npcdata))
		w.u(uint64(r.Nfuncdata))
		prev = r.Idx
	}
	names := sortedNonEmpty(l.Shifts)
	w.u(uint64(len(names)))
	for _, n := range names {
		w.str(n)
		w.bytes(l.Shifts[n].encode())
	}
	names = sortedKeys(l.DataMaps)
	w.u(uint64(len(names)))
	for _, n := range names {
		w.str(n)
		w.bytes(l.DataMaps[n].encodeRLE())
	}
	encodeOverrides(w, l.Overrides)
	return w.b
}

func decodeLayout(b []byte, old *gobin.Bin) (*layout, error) {
	r := &rbuf{b: b}
	if string(r.take(4)) != layoutMagic {
		return nil, fmt.Errorf("%w: bad layout magic", errCorrupt)
	}
	l := &layout{Shifts: map[string]*shiftTable{}, DataMaps: map[string]*dataMap{}}
	l.NewLen = uint64(int64(len(old.File)) + r.s())
	l.Sects = decodeSects(r, old)
	for _, v := range l.modValues(old.Mod) {
		*v[0] = uint64(int64(*v[1]) + r.s())
	}
	l.FirstEntry = uint64(int64(old.Funcs[0].Entry) + r.s())
	l.NFunc = int(r.un(maxFuncs, "function count"))
	l.Funcs = r.bytes()
	l.NFiles = uint64(int64(old.Pcln.NFiles) + r.s())
	l.PclnLen = uint64(int64(len(old.Pcln.Data)) + r.s())
	ot := oldTabOffs(old.Pcln)
	for i := range l.TabOff {
		l.TabOff[i] = uint64(int64(ot[i]) + r.s())
	}
	n := r.un(maxFuncs, "record shape count")
	prev := 0
	for i := uint64(0); i < n && r.err == nil; i++ {
		idx := prev + int(r.un(maxFuncs, "record shape index"))
		l.RecShapes = append(l.RecShapes, recShape{idx, uint32(r.un(1<<16, "npcdata")), uint32(r.un(256, "nfuncdata"))})
		prev = idx
	}
	n = r.un(64, "shift table count")
	for i := uint64(0); i < n && r.err == nil; i++ {
		name := string(r.take(r.un(256, "section name length")))
		st, err := decodeShiftTable(r.bytes())
		if err != nil {
			return nil, err
		}
		l.Shifts[name] = st
	}
	n = r.un(64, "data map count")
	for i := uint64(0); i < n && r.err == nil; i++ {
		name := string(r.take(r.un(256, "section name length")))
		dm, err := decodeDataMapRLE(r.bytes())
		if err != nil {
			return nil, err
		}
		l.DataMaps[name] = dm
	}
	l.Overrides = decodeOverrides(r)
	if err := r.done(); err != nil {
		return nil, err
	}
	if l.NewLen > maxPatchSize {
		return nil, fmt.Errorf("%w: layout claims a %d-byte file", errCorrupt, l.NewLen)
	}
	return l, nil
}

func encodeOverrides(w *wbuf, ov []addrOverride) {
	w.u(uint64(len(ov)))
	var po uint64
	var pd int64
	for _, o := range ov {
		w.u(o.Old - po)
		d := int64(o.New) - int64(o.Old)
		w.s(d - pd)
		po, pd = o.Old, d
	}
}

func decodeOverrides(r *rbuf) []addrOverride {
	n := r.un(uint64(len(r.b))+1, "override count")
	ov := make([]addrOverride, 0, n)
	var po uint64
	var pd int64
	for i := uint64(0); i < n && r.err == nil; i++ {
		po += r.u()
		pd += r.s()
		ov = append(ov, addrOverride{po, uint64(int64(po) + pd)})
	}
	return ov
}

func overrideMap(ov []addrOverride) map[uint64]uint64 {
	if len(ov) == 0 {
		return nil
	}
	m := make(map[uint64]uint64, len(ov))
	for _, o := range ov {
		m[o.Old] = o.New
	}
	return m
}

func sortedNonEmpty(m map[string]*shiftTable) []string {
	var ks []string
	for k, v := range m {
		if v != nil && len(v.Offs) > 0 {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

func sortedKeys(m map[string]*dataMap) []string {
	var ks []string
	for k, v := range m {
		if v != nil {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

// encodeRLE serialises the per-block shifts as runs, since a section that
// gained one string is one shift change for hundreds of thousands of blocks.
func (m *dataMap) encodeRLE() []byte {
	w := &wbuf{}
	w.u(uint64(m.Block))
	w.u(uint64(m.OldLen))
	type run struct {
		n int
		d int64
	}
	var runs []run
	for i, d := range m.Delta {
		if i > 0 && runs[len(runs)-1].d == d {
			runs[len(runs)-1].n++
			continue
		}
		runs = append(runs, run{1, d})
	}
	w.u(uint64(len(runs)))
	var prev int64
	for _, r := range runs {
		w.u(uint64(r.n))
		w.s(r.d - prev)
		prev = r.d
	}
	return w.b
}

func decodeDataMapRLE(b []byte) (*dataMap, error) {
	r := &rbuf{b: b}
	m := &dataMap{Block: int(r.un(1<<16, "data map block size")), OldLen: int(r.un(maxPatchSize, "data map source length"))}
	if m.Block == 0 {
		return nil, fmt.Errorf("%w: data map block size 0", errCorrupt)
	}
	nruns := r.un(uint64(len(b))+1, "data map run count")
	nb := (m.OldLen + m.Block - 1) / m.Block
	m.Delta = make([]int64, 0, nb)
	var prev int64
	for i := uint64(0); i < nruns && r.err == nil; i++ {
		n := r.un(uint64(nb+1), "data map run length")
		prev += r.s()
		if len(m.Delta)+int(n) > nb {
			return nil, fmt.Errorf("%w: data map runs cover more than %d blocks", errCorrupt, nb)
		}
		for range n {
			m.Delta = append(m.Delta, prev)
		}
	}
	if err := r.done(); err != nil {
		return nil, err
	}
	m.Matched = make([]bool, len(m.Delta))
	return m, nil
}

// encodeFuncLayout writes the new function list as a delta against the old
// one: op 0 = the next old function, op 1 = an explicit old index, op 2 = a
// name that is new; then the size change. In a normal release almost every
// function is op 0 with a zero size delta, which is why 110 K functions cost
// 11 KB.
func encodeFuncLayout(old, new *gobin.Bin, m *match) []byte {
	w := &wbuf{}
	expect := 0
	for j, f := range new.Funcs {
		i := m.NewToOld[j]
		var oldSize int64
		switch {
		case i < 0:
			w.raw([]byte{2})
			w.str(f.Name)
		case i == expect:
			w.raw([]byte{0})
			oldSize = int64(old.Funcs[i].Size())
			expect = i + 1
		default:
			w.raw([]byte{1})
			w.u(uint64(i))
			oldSize = int64(old.Funcs[i].Size())
			expect = i + 1
		}
		w.s(int64(f.Size()) - oldSize)
	}
	return w.b
}

func decodeFuncLayout(old *gobin.Bin, l *layout) ([]*gobin.Func, *match, error) {
	r := &rbuf{b: l.Funcs}
	funcs := make([]*gobin.Func, 0, l.NFunc)
	m := &match{NewToOld: make([]int, l.NFunc), OldToNew: make([]int, len(old.Funcs))}
	for i := range m.OldToNew {
		m.OldToNew[i] = -1
	}
	expect := 0
	entry := l.FirstEntry
	for j := 0; j < l.NFunc; j++ {
		i := -1
		var name string
		var oldSize int64
		switch op := r.byte(); op {
		case 0:
			i = expect
		case 1:
			i = int(r.un(uint64(len(old.Funcs)), "old function index"))
		case 2:
			name = string(r.take(r.un(1<<16, "function name length")))
		default:
			return nil, nil, fmt.Errorf("%w: bad function op %d", errCorrupt, op)
		}
		if r.err != nil {
			return nil, nil, r.err
		}
		if i >= 0 {
			if i >= len(old.Funcs) {
				return nil, nil, fmt.Errorf("%w: old function index %d out of range", errCorrupt, i)
			}
			name = old.Funcs[i].Name
			oldSize = int64(old.Funcs[i].Size())
			expect = i + 1
			m.OldToNew[i] = j
		}
		size := oldSize + r.s()
		if r.err != nil {
			return nil, nil, r.err
		}
		if size < 0 || entry+uint64(size) < entry {
			return nil, nil, fmt.Errorf("%w: function %d has size %d", errCorrupt, j, size)
		}
		funcs = append(funcs, &gobin.Func{Idx: j, Name: name, Entry: entry, End: entry + uint64(size)})
		m.NewToOld[j] = i
		entry += uint64(size)
	}
	if err := r.done(); err != nil {
		return nil, nil, err
	}
	for j := range m.NewToOld {
		if m.NewToOld[j] < 0 {
			m.Unmatched++
		} else {
			m.Exact++
		}
	}
	for i := range m.OldToNew {
		if m.OldToNew[i] < 0 {
			m.UnmatchedOld++
		}
	}
	return funcs, m, nil
}

// buildLayout is the encoder side.
func buildLayout(old, new *gobin.Bin, m *match, dmaps map[string]*dataMap, shifts map[string]*shiftTable, ov []addrOverride) *layout {
	l := &layout{NewLen: uint64(len(new.File)), Shifts: shifts, DataMaps: dmaps, Overrides: ov}
	for _, s := range new.Order {
		l.Sects = append(l.Sects, sectInfo{s.Name, s.Addr, s.Off, s.Size, s.NoBits})
	}
	nm := new.Mod
	l.Text, l.Types, l.Etypes, l.Typedesclen, l.Itaboffset, l.Itabsize = nm.Text, nm.Types, nm.Etypes, nm.Typedesclen, nm.Itaboffset, nm.Itabsize
	l.Rodata, l.Gofunc, l.Findfunctab, l.Epclntab, l.Minpc, l.Maxpc = nm.Rodata, nm.Gofunc, nm.Findfunctab, nm.Epclntab, nm.Minpc, nm.Maxpc
	l.FirstEntry = new.Funcs[0].Entry
	l.NFunc = len(new.Funcs)
	l.Funcs = encodeFuncLayout(old, new, m)
	np := new.Pcln
	l.NFiles = uint64(np.NFiles)
	l.PclnLen = uint64(len(np.Data))
	l.TabOff = oldTabOffs(np)
	// only the records whose shape the decoder's default gets wrong
	mode := gobin.ModalShape(old)
	for j, g := range new.Funcs {
		npc, nfd, _ := np.Record(g.FuncOff)
		def := mode
		if i := m.NewToOld[j]; i >= 0 {
			a, b, _ := old.Pcln.Record(old.Funcs[i].FuncOff)
			def = [2]uint32{a, b}
		}
		if def != [2]uint32{npc, nfd} {
			l.RecShapes = append(l.RecShapes, recShape{j, npc, nfd})
		}
	}
	return l
}

// skeleton is the decoder's view of the new binary: the section table, the
// moduledata values, the function list and a pclntab of the right length
// whose tables are filled in by stage 1. Nothing of the real new file is
// read, which is the point -- the predictors run identically on both sides.
func skeleton(old *gobin.Bin, l *layout) (*gobin.Bin, *match, error) {
	b := &gobin.Bin{Sects: map[string]*gobin.Section{}, GoVer: old.GoVer}
	for _, s := range l.Sects {
		if !s.NoBits && (s.Off > l.NewLen || s.Size > l.NewLen || s.Off+s.Size > l.NewLen) {
			return nil, nil, fmt.Errorf("%w: section %s runs past the new file", errCorrupt, s.Name)
		}
		sec := &gobin.Section{Name: s.Name, Addr: s.Addr, Off: s.Off, Size: s.Size, NoBits: s.NoBits}
		b.Sects[s.Name] = sec
		b.Order = append(b.Order, sec)
	}
	sort.Slice(b.Order, func(i, j int) bool { return b.Order[i].Addr < b.Order[j].Addr })
	b.Text = b.Sects[".text"]
	if b.Text == nil || b.Sects[".gopclntab"] == nil {
		return nil, nil, fmt.Errorf("%w: layout has no .text or .gopclntab", errCorrupt)
	}
	b.Mod = &gobin.Moduledata{
		Text: l.Text, Types: l.Types, Etypes: l.Etypes, Typedesclen: l.Typedesclen,
		Itaboffset: l.Itaboffset, Itabsize: l.Itabsize, Rodata: l.Rodata, Gofunc: l.Gofunc,
		Findfunctab: l.Findfunctab, Epclntab: l.Epclntab, Minpc: l.Minpc, Maxpc: l.Maxpc,
	}
	funcs, m, err := decodeFuncLayout(old, l)
	if err != nil {
		return nil, nil, err
	}
	if len(funcs) == 0 {
		return nil, nil, fmt.Errorf("%w: layout has no functions", errCorrupt)
	}
	b.Funcs = funcs
	if last := funcs[len(funcs)-1]; last.End > b.Text.Addr+b.Text.Size || funcs[0].Entry < b.Text.Addr {
		return nil, nil, fmt.Errorf("%w: the function layout does not fit .text", errCorrupt)
	}
	p := &gobin.Pcln{
		Addr: b.Sects[".gopclntab"].Addr, NFunc: l.NFunc, NFiles: int(l.NFiles),
		MinLC: old.Pcln.MinLC, PtrSize: 8,
	}
	t := l.TabOff
	p.FuncnameOff, p.CuOff, p.FiletabOff, p.PctabOff, p.FunctabOff = t[0], t[1], t[2], t[3], t[4]
	p.Funcnametab = gobin.Range{Off: t[0], Len: t[1] - t[0]}
	p.Cutab = gobin.Range{Off: t[1], Len: t[2] - t[1]}
	p.Filetab = gobin.Range{Off: t[2], Len: t[3] - t[2]}
	p.Pctab = gobin.Range{Off: t[3], Len: t[4] - t[3]}
	p.Functab = gobin.Range{Off: t[4], Len: t[5] - t[4]}
	p.Gofunc = gobin.Range{Off: t[5], Len: t[6] - t[5]}
	p.Findfunctab = gobin.Range{Off: t[6], Len: l.PclnLen - t[6]}
	for i, r := range []gobin.Range{p.Funcnametab, p.Cutab, p.Filetab, p.Pctab, p.Functab, p.Gofunc, p.Findfunctab} {
		if r.Off > r.End() || r.End() > l.PclnLen {
			return nil, nil, fmt.Errorf("%w: pclntab table %d is outside the %d-byte section", errCorrupt, i, l.PclnLen)
		}
	}
	if l.PclnLen < 72 {
		return nil, nil, fmt.Errorf("%w: pclntab is %d bytes", errCorrupt, l.PclnLen)
	}
	p.Data = make([]byte, l.PclnLen)
	copy(p.Data[:72], old.Pcln.Data[:72])
	b.Pcln = p
	return b, m, nil
}

// stage1aRanges and stage1bRanges split the pclntab tables that are
// transmitted from the ones that are predicted.
func stage1aRanges(p *gobin.Pcln) []gobin.Range { return []gobin.Range{p.Funcnametab, p.Filetab} }
func stage1bRanges(p *gobin.Pcln) []gobin.Range { return []gobin.Range{p.Cutab, p.Pctab, p.Gofunc} }

// fillTables copies transmitted table bytes into the skeleton's pclntab at
// their final offsets, so the predictors read them exactly as they would
// from the real file.
func fillTables(b *gobin.Bin, ranges []gobin.Range, data []byte) error {
	var want uint64
	for _, r := range ranges {
		want += r.Len
	}
	if uint64(len(data)) != want {
		return fmt.Errorf("%w: stage-1 tables are %d bytes, the layout implies %d", errCorrupt, len(data), want)
	}
	var o uint64
	for _, r := range ranges {
		copy(b.Pcln.Data[r.Off:r.End()], data[o:o+r.Len])
		o += r.Len
	}
	return nil
}

func concatRanges(b *gobin.Bin, ranges []gobin.Range) []byte {
	var out []byte
	for _, r := range ranges {
		out = append(out, b.Pcln.Table(r)...)
	}
	return out
}
