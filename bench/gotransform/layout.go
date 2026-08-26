package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// Layout is everything the decoder needs besides the old binary and the
// stage-1 blobs: the new file's length and section table, the moduledata
// values the predictors read, the function layout table, the pclntab table
// offsets and per-record shapes, the bss shift tables and the data-section
// content maps. It is serialised with varints and zstd-compressed; its size
// is counted in every total.
type Layout struct {
	GoLayout string
	NewLen   uint64
	Sects    []SectInfo
	// moduledata values used by the predictors
	Text, Types, Etypes, Typedesclen, Itaboffset, Itabsize uint64
	Rodata, Gofunc, Findfunctab, Epclntab, Minpc, Maxpc    uint64
	// function layout: first entry address, then encodeLayout's op stream
	FirstEntry uint64
	NFunc      int
	Funcs      []byte
	// pclntab: header nfiles, total length, and the offsets of the seven
	// tables (funcnametab, cutab, filetab, pctab, functab, go:func.*,
	// findfunctab); each table runs to the next one, so the stage-1 blob
	// lengths follow from these.
	NFiles  uint64
	PclnLen uint64
	TabOff  [7]uint64
	// (npcdata, nfuncdata) for every new-function record and for matched
	// records whose shape changed, so the rebuilt functab is length-exact.
	RecShapes []RecShape
	Shifts    map[string]*ShiftTable
	DataMaps  map[string]*DataMap // only Block/OldLen/Delta are transmitted
	// pointer-target overrides: old address -> new address for targets
	// whose content-map prediction is wrong (chosen by vote at the encoder,
	// see deriveOverrides); consulted by mapAddr before the maps
	Overrides []AddrOverride
}

type AddrOverride struct{ Old, New uint64 }

type SectInfo struct {
	Name            string
	Addr, Off, Size uint64
	NoBits          bool
}

// encodeSects writes the new section table as deltas against the old one:
// per section, op 0 = same name as old section k (implicit next), 1 = same
// name as explicit old index, 2 = new name; then zigzag deltas of addr/off/
// size against the old values (or absolute for a new section).
func encodeSects(w *wbuf, old *Bin, sects []SectInfo) {
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
			w.b = append(w.b, 2)
			w.str(s.Name)
		case k == expect:
			w.b = append(w.b, 0)
			expect++
		default:
			w.b = append(w.b, 1)
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
		w.b = append(w.b, nb)
	}
}

func decodeSects(r *rbuf, old *Bin) []SectInfo {
	n := int(r.u())
	var out []SectInfo
	expect := 0
	for i := 0; i < n && r.err == nil; i++ {
		if len(r.b) == 0 {
			r.err = fmt.Errorf("layout: section stream truncated")
			return nil
		}
		op := r.b[0]
		r.b = r.b[1:]
		k := -1
		var s SectInfo
		switch op {
		case 0:
			k = expect
		case 1:
			k = int(r.u())
		case 2:
			s.Name = r.str()
		default:
			r.err = fmt.Errorf("layout: bad section op %d", op)
			return nil
		}
		var oa, oo, osz uint64
		if k >= 0 {
			if k >= len(old.Order) {
				r.err = fmt.Errorf("layout: old section index %d out of range", k)
				return nil
			}
			s.Name = old.Order[k].Name
			oa, oo, osz = old.Order[k].Addr, old.Order[k].Off, old.Order[k].Size
			expect = k + 1
		}
		s.Addr = uint64(int64(oa) + r.s())
		s.Off = uint64(int64(oo) + r.s())
		s.Size = uint64(int64(osz) + r.s())
		if len(r.b) == 0 {
			r.err = fmt.Errorf("layout: section stream truncated")
			return nil
		}
		s.NoBits = r.b[0] == 1
		r.b = r.b[1:]
		out = append(out, s)
	}
	return out
}

type RecShape struct {
	Idx                int
	Npcdata, Nfuncdata uint32
}

// dataMapSects are the sections predicted through a content map.
var dataMapSects = []string{".rodata", ".go.type", ".go.func", ".noptrdata", ".data"}

// ptrRewriteSects are copied and have their absolute pointers re-targeted
// (identity content map).
var ptrRewriteSects = []string{".go.module", ".dynamic", ".got", ".got.plt", ".rela", ".rela.plt", ".go.fipsinfo", ".dynsym"}

type wbuf struct{ b []byte }

func (w *wbuf) u(v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	w.b = append(w.b, tmp[:n]...)
}
func (w *wbuf) s(v int64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutVarint(tmp[:], v)
	w.b = append(w.b, tmp[:n]...)
}
func (w *wbuf) str(s string)   { w.u(uint64(len(s))); w.b = append(w.b, s...) }
func (w *wbuf) bytes(b []byte) { w.u(uint64(len(b))); w.b = append(w.b, b...) }

type rbuf struct {
	b   []byte
	err error
}

func (r *rbuf) u() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("layout: bad uvarint")
		r.b = nil
		return 0
	}
	r.b = r.b[n:]
	return v
}
func (r *rbuf) s() int64 {
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("layout: bad varint")
		r.b = nil
		return 0
	}
	r.b = r.b[n:]
	return v
}
func (r *rbuf) str() string { return string(r.bytes()) }
func (r *rbuf) bytes() []byte {
	n := r.u()
	if uint64(len(r.b)) < n {
		r.err = fmt.Errorf("layout: short bytes")
		r.b = nil
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// Encode serialises the layout (uncompressed) as a delta against old.
func (l *Layout) Encode(old *Bin) []byte {
	w := &wbuf{}
	w.str("GTL1")
	w.str(l.GoLayout)
	w.s(int64(l.NewLen) - int64(len(old.File)))
	encodeSects(w, old, l.Sects)
	om := old.Mod
	for _, v := range [][2]uint64{{l.Text, om.Text}, {l.Types, om.Types}, {l.Etypes, om.Etypes}, {l.Typedesclen, om.Typedesclen}, {l.Itaboffset, om.Itaboffset}, {l.Itabsize, om.Itabsize}, {l.Rodata, om.Rodata}, {l.Gofunc, om.Gofunc}, {l.Findfunctab, om.Findfunctab}, {l.Epclntab, om.Epclntab}, {l.Minpc, om.Minpc}, {l.Maxpc, om.Maxpc}} {
		w.s(int64(v[0]) - int64(v[1]))
	}
	w.s(int64(l.FirstEntry) - int64(old.Funcs[0].Entry))
	w.u(uint64(l.NFunc))
	w.bytes(l.Funcs)
	w.s(int64(l.NFiles) - int64(old.Pcln.NFiles))
	w.s(int64(l.PclnLen) - int64(len(old.Pcln.Data)))
	op := old.Pcln
	for i, v := range l.TabOff {
		w.s(int64(v) - int64([7]uint64{op.Funcnametab.Off, op.Cutab.Off, op.Filetab.Off, op.Pctab.Off, op.Functab.Off, op.Gofunc.Off, op.Findfunctab.Off}[i]))
	}
	w.u(uint64(len(l.RecShapes)))
	prev := 0
	for _, r := range l.RecShapes {
		w.u(uint64(r.Idx - prev))
		w.u(uint64(r.Npcdata))
		w.u(uint64(r.Nfuncdata))
		prev = r.Idx
	}
	names := sortedKeys(l.Shifts)
	w.u(uint64(len(names)))
	for _, n := range names {
		w.str(n)
		w.bytes(l.Shifts[n].Encode())
	}
	names = sortedKeysDM(l.DataMaps)
	w.u(uint64(len(names)))
	for _, n := range names {
		w.str(n)
		w.bytes(l.DataMaps[n].EncodeRLE())
	}
	encodeOverrides(w, l.Overrides)
	return w.b
}

// encodeOverrides writes the override table (sorted by old address) as the
// gap since the previous old address and the change of (new - old) since
// the previous entry.
func encodeOverrides(w *wbuf, ov []AddrOverride) {
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

func decodeOverrides(r *rbuf) []AddrOverride {
	n := r.u()
	if n > uint64(len(r.b)) {
		r.err = fmt.Errorf("layout: %d overrides in %d bytes", n, len(r.b))
		return nil
	}
	ov := make([]AddrOverride, 0, n)
	var po uint64
	var pd int64
	for i := uint64(0); i < n && r.err == nil; i++ {
		po += r.u()
		pd += r.s()
		ov = append(ov, AddrOverride{po, uint64(int64(po) + pd)})
	}
	return ov
}

func sortedKeys(m map[string]*ShiftTable) []string {
	var ks []string
	for k, v := range m {
		if v != nil && len(v.Offs) > 0 {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

func sortedKeysDM(m map[string]*DataMap) []string {
	var ks []string
	for k, v := range m {
		if v != nil {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	return ks
}

// DecodeLayout parses Encode's output.
func DecodeLayout(b []byte, old *Bin) (*Layout, error) {
	r := &rbuf{b: b}
	if r.str() != "GTL1" {
		return nil, fmt.Errorf("layout: bad magic")
	}
	l := &Layout{Shifts: map[string]*ShiftTable{}, DataMaps: map[string]*DataMap{}}
	l.GoLayout = r.str()
	l.NewLen = uint64(int64(len(old.File)) + r.s())
	l.Sects = decodeSects(r, old)
	if r.err != nil {
		return nil, r.err
	}
	om := old.Mod
	for _, p := range [][2]any{{&l.Text, om.Text}, {&l.Types, om.Types}, {&l.Etypes, om.Etypes}, {&l.Typedesclen, om.Typedesclen}, {&l.Itaboffset, om.Itaboffset}, {&l.Itabsize, om.Itabsize}, {&l.Rodata, om.Rodata}, {&l.Gofunc, om.Gofunc}, {&l.Findfunctab, om.Findfunctab}, {&l.Epclntab, om.Epclntab}, {&l.Minpc, om.Minpc}, {&l.Maxpc, om.Maxpc}} {
		*p[0].(*uint64) = uint64(int64(p[1].(uint64)) + r.s())
	}
	l.FirstEntry = uint64(int64(old.Funcs[0].Entry) + r.s())
	l.NFunc = int(r.u())
	l.Funcs = append([]byte(nil), r.bytes()...)
	l.NFiles = uint64(int64(old.Pcln.NFiles) + r.s())
	l.PclnLen = uint64(int64(len(old.Pcln.Data)) + r.s())
	op := old.Pcln
	for i := range l.TabOff {
		l.TabOff[i] = uint64(int64([7]uint64{op.Funcnametab.Off, op.Cutab.Off, op.Filetab.Off, op.Pctab.Off, op.Functab.Off, op.Gofunc.Off, op.Findfunctab.Off}[i]) + r.s())
	}
	n := int(r.u())
	prev := 0
	for i := 0; i < n && r.err == nil; i++ {
		idx := prev + int(r.u())
		l.RecShapes = append(l.RecShapes, RecShape{idx, uint32(r.u()), uint32(r.u())})
		prev = idx
	}
	n = int(r.u())
	for i := 0; i < n && r.err == nil; i++ {
		name := r.str()
		st, err := DecodeShiftTable(r.bytes())
		if err != nil {
			return nil, err
		}
		l.Shifts[name] = st
	}
	n = int(r.u())
	for i := 0; i < n && r.err == nil; i++ {
		name := r.str()
		dm, err := DecodeDataMapRLE(r.bytes())
		if err != nil {
			return nil, err
		}
		l.DataMaps[name] = dm
	}
	l.Overrides = decodeOverrides(r)
	if r.err != nil {
		return nil, r.err
	}
	return l, nil
}

// overrideMap is the mapper's view of the override table.
func overrideMap(ov []AddrOverride) map[uint64]uint64 {
	if len(ov) == 0 {
		return nil
	}
	m := make(map[uint64]uint64, len(ov))
	for _, o := range ov {
		m[o.Old] = o.New
	}
	return m
}

// EncodeRLE serialises the per-block shifts as runs (count, zigzag delta
// difference); one pair per shift change.
func (m *DataMap) EncodeRLE() []byte {
	w := &wbuf{}
	w.u(uint64(m.Block))
	w.u(uint64(m.OldLen))
	var runs []struct {
		n int
		d int64
	}
	for i, d := range m.Delta {
		if i > 0 && runs[len(runs)-1].d == d {
			runs[len(runs)-1].n++
			continue
		}
		runs = append(runs, struct {
			n int
			d int64
		}{1, d})
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

func DecodeDataMapRLE(b []byte) (*DataMap, error) {
	r := &rbuf{b: b}
	m := &DataMap{Block: int(r.u()), OldLen: int(r.u())}
	nruns := int(r.u())
	var prev int64
	for i := 0; i < nruns && r.err == nil; i++ {
		n := int(r.u())
		prev += r.s()
		for k := 0; k < n; k++ {
			m.Delta = append(m.Delta, prev)
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	m.Matched = make([]bool, len(m.Delta))
	return m, nil
}

// DecodeShiftTable parses ShiftTable.Encode's output.
func DecodeShiftTable(b []byte) (*ShiftTable, error) {
	r := &rbuf{b: b}
	t := &ShiftTable{}
	var po uint64
	var pd int64
	for len(r.b) > 0 && r.err == nil {
		po += r.u()
		pd += r.s()
		t.Offs = append(t.Offs, po)
		t.Deltas = append(t.Deltas, pd)
	}
	return t, r.err
}

// buildLayout is the encoder side: it derives the layout from old, new and
// the match, plus the content maps and shift tables already built.
func buildLayout(old, new *Bin, m *Match, dmaps map[string]*DataMap, shifts map[string]*ShiftTable, overrides []AddrOverride) *Layout {
	l := &Layout{GoLayout: new.Layout, NewLen: uint64(len(new.File)), Shifts: shifts, DataMaps: dmaps, Overrides: overrides}
	for _, s := range new.Order {
		l.Sects = append(l.Sects, SectInfo{s.Name, s.Addr, s.Off, s.Size, s.NoBits})
	}
	md := new.Mod
	l.Text, l.Types, l.Etypes, l.Typedesclen, l.Itaboffset, l.Itabsize = md.Text, md.Types, md.Etypes, md.Typedesclen, md.Itaboffset, md.Itabsize
	l.Rodata, l.Gofunc, l.Findfunctab, l.Epclntab, l.Minpc, l.Maxpc = md.Rodata, md.Gofunc, md.Findfunctab, md.Epclntab, md.Minpc, md.Maxpc
	l.FirstEntry = new.Funcs[0].Entry
	l.NFunc = len(new.Funcs)
	l.Funcs = encodeLayout(old, new, m)
	np := new.Pcln
	l.NFiles = uint64(np.NFiles)
	l.PclnLen = uint64(len(np.Data))
	l.TabOff = [7]uint64{np.Funcnametab.Off, np.Cutab.Off, np.Filetab.Off, np.Pctab.Off, np.Functab.Off, np.Gofunc.Off, np.Findfunctab.Off}
	// record shapes: the decoder's default is the old record's shape for a
	// matched function and the old binary's modal shape for a new one
	mode := modalShape(old)
	for j, g := range new.Funcs {
		npc, nfd, _ := np.funcRecord(g.FuncOff)
		def := mode
		if i := m.NewToOld[j]; i >= 0 {
			a, b, _ := old.Pcln.funcRecord(old.Funcs[i].FuncOff)
			def = [2]uint32{a, b}
		}
		if def != [2]uint32{npc, nfd} {
			l.RecShapes = append(l.RecShapes, RecShape{j, npc, nfd})
		}
	}
	return l
}

func modalShape(b *Bin) [2]uint32 {
	cnt := map[[2]uint32]int{}
	for _, f := range b.Funcs {
		np, nf, _ := b.Pcln.funcRecord(f.FuncOff)
		cnt[[2]uint32{np, nf}]++
	}
	var best [2]uint32
	bestN := -1
	for k, c := range cnt {
		if c > bestN || c == bestN && (k[0] < best[0] || k[0] == best[0] && k[1] < best[1]) {
			best, bestN = k, c
		}
	}
	return best
}

// decodeFuncLayout rebuilds the new function list from the old one and the
// layout op stream (see encodeLayout).
func decodeFuncLayout(old *Bin, l *Layout) ([]*Func, *Match, error) {
	r := &rbuf{b: l.Funcs}
	funcs := make([]*Func, 0, l.NFunc)
	m := &Match{NewToOld: make([]int, l.NFunc), OldToNew: make([]int, len(old.Funcs)), How: make([]byte, l.NFunc)}
	for i := range m.OldToNew {
		m.OldToNew[i] = -1
	}
	expect := 0
	entry := l.FirstEntry
	for j := 0; j < l.NFunc; j++ {
		if len(r.b) == 0 {
			return nil, nil, fmt.Errorf("layout: func stream truncated at %d", j)
		}
		op := r.b[0]
		r.b = r.b[1:]
		i := -1
		var name string
		var oldSize int64
		switch op {
		case 0:
			i = expect
		case 1:
			i = int(r.u())
		case 2:
			name = r.str()
		default:
			return nil, nil, fmt.Errorf("layout: bad func op %d", op)
		}
		if i >= 0 {
			if i >= len(old.Funcs) {
				return nil, nil, fmt.Errorf("layout: old index %d out of range", i)
			}
			name = old.Funcs[i].Name
			oldSize = int64(old.Funcs[i].Size())
			expect = i + 1
			m.OldToNew[i] = j
			m.How[j] = 'e'
		}
		size := oldSize + r.s()
		if r.err != nil {
			return nil, nil, r.err
		}
		f := &Func{Idx: j, Name: name, Entry: entry, End: entry + uint64(size)}
		funcs = append(funcs, f)
		m.NewToOld[j] = i
		entry = f.End
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

// skeletonBin builds the decoder's view of the new binary from the old one
// and the layout. Sections carry no data; the fake pclntab is zeroed and
// receives the stage-1 tables at their final offsets (fillSkeleton) so that
// the predictors can read them exactly as they would from the real file.
func skeletonBin(old *Bin, l *Layout) (*Bin, *Match, error) {
	b := &Bin{Path: "<skeleton>", Sects: map[string]*Section{}, Layout: l.GoLayout}
	for _, s := range l.Sects {
		sec := &Section{Name: s.Name, Addr: s.Addr, Off: s.Off, Size: s.Size, NoBits: s.NoBits}
		b.Sects[s.Name] = sec
		b.Order = append(b.Order, sec)
	}
	sort.Slice(b.Order, func(i, j int) bool { return b.Order[i].Addr < b.Order[j].Addr })
	b.Text = b.Sects[".text"]
	if b.Text == nil || b.Sects[".gopclntab"] == nil {
		return nil, nil, fmt.Errorf("skeleton: no .text/.gopclntab in layout")
	}
	b.Mod = &Moduledata{Text: l.Text, Types: l.Types, Etypes: l.Etypes, Typedesclen: l.Typedesclen, Itaboffset: l.Itaboffset, Itabsize: l.Itabsize,
		Rodata: l.Rodata, Gofunc: l.Gofunc, Findfunctab: l.Findfunctab, Epclntab: l.Epclntab, Minpc: l.Minpc, Maxpc: l.Maxpc}
	funcs, m, err := decodeFuncLayout(old, l)
	if err != nil {
		return nil, nil, err
	}
	b.Funcs = funcs
	b.ByName = map[string][]*Func{}
	for _, f := range funcs {
		b.ByName[f.Name] = append(b.ByName[f.Name], f)
	}
	p := &Pcln{Addr: b.Sects[".gopclntab"].Addr, NFunc: l.NFunc, NFiles: int(l.NFiles), MinLC: old.Pcln.MinLC, PtrSize: 8, Inside: old.Pcln.Inside}
	t := l.TabOff
	p.FuncnameOff, p.CuOff, p.FiletabOff, p.PctabOff, p.FunctabOff = t[0], t[1], t[2], t[3], t[4]
	p.Funcnametab = Range{t[0], t[1] - t[0]}
	p.Cutab = Range{t[1], t[2] - t[1]}
	p.Filetab = Range{t[2], t[3] - t[2]}
	p.Pctab = Range{t[3], t[4] - t[3]}
	p.Functab = Range{t[4], t[5] - t[4]}
	p.Gofunc = Range{t[5], t[6] - t[5]}
	p.Findfunctab = Range{t[6], l.PclnLen - t[6]}
	for _, r := range []Range{p.Funcnametab, p.Cutab, p.Filetab, p.Pctab, p.Gofunc, p.Findfunctab} {
		if r.End() > l.PclnLen || r.Off > r.End() {
			return nil, nil, fmt.Errorf("skeleton: pclntab table %+v outside %d", r, l.PclnLen)
		}
	}
	p.Data = make([]byte, l.PclnLen)
	copy(p.Data[:72], old.Pcln.Data[:72])
	b.Pcln = p
	return b, m, nil
}

func (p *Pcln) ranges1a() []Range { return []Range{p.Funcnametab, p.Filetab} }
func (p *Pcln) ranges1b() []Range { return []Range{p.Cutab, p.Pctab, p.Gofunc} }

// layoutEqual is a debugging aid: it checks that a layout survives a
// round trip.
func layoutRoundTrip(l *Layout, old *Bin) error {
	enc := l.Encode(old)
	d, err := DecodeLayout(enc, old)
	if err != nil {
		return err
	}
	if !bytes.Equal(d.Encode(old), enc) {
		return fmt.Errorf("layout round trip differs")
	}
	return nil
}
