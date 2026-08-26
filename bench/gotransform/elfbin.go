package main

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Section is one ELF section with its bytes.
type Section struct {
	Name   string
	Addr   uint64
	Off    uint64
	Size   uint64
	Data   []byte // nil for NOBITS (and in a decoder-side skeleton)
	NoBits bool
}

// Func is one entry of the pclntab function table.
type Func struct {
	Idx     int
	Name    string
	Entry   uint64 // absolute address
	End     uint64 // next entry (or end pc for the last one)
	FuncOff uint32 // offset of the _func record from functab start
	NameOff int32
}

func (f *Func) Size() uint64 { return f.End - f.Entry }

// Bin is a loaded Go ELF binary.
type Bin struct {
	Path   string
	File   []byte
	Sects  map[string]*Section
	Order  []*Section // by address, allocated sections only
	Text   *Section
	Pcln   *Pcln
	Mod    *Moduledata
	Funcs  []*Func // in functab order (== address order)
	ByName map[string][]*Func
	Layout string // "go1.27" (types/itabs in .go.type, no typelinks), "go1.26" (.go.module, go:func.*/findfunctab inside .gopclntab) or "go1.25" (moduledata in .noptrdata, funcdata/findfunctab in .rodata)
	GoVer  string // from .go.buildinfo, e.g. "go1.27.0"
}

// Pcln is a parsed Go 1.20-format pclntab (magic 0xfffffff1).
type Pcln struct {
	Data    []byte
	Addr    uint64
	NFunc   int
	NFiles  int
	MinLC   uint8
	PtrSize uint8
	// offsets of the sub-tables from the start of the section (from pcHeader)
	FuncnameOff, CuOff, FiletabOff, PctabOff, FunctabOff uint64
	// bounds from moduledata (start offset, length) of the sub-tables
	Funcnametab, Cutab, Filetab, Pctab, Functab, Gofunc, Findfunctab Range
	// Go 1.25: go:func.* and findfunctab live in .rodata; the ranges above are
	// then relative to that section and Inside is false.
	Inside bool
}

type Range struct{ Off, Len uint64 }

func (r Range) End() uint64 { return r.Off + r.Len }

// Moduledata is the subset of runtime.firstmoduledata we need (Go 1.26 layout).
type Moduledata struct {
	PcHeader                                      uint64
	Funcnametab, Cutab, Filetab, Pctab, Pclntable Slice
	Ftab                                          Slice
	Findfunctab                                   uint64
	Minpc, Maxpc                                  uint64
	Text, Etext                                   uint64
	Noptrdata, Enoptrdata                         uint64
	Data, Edata                                   uint64
	Bss, Ebss                                     uint64
	Noptrbss, Enoptrbss                           uint64
	Types, Etypes                                 uint64
	Typedesclen, Itaboffset, Itabsize             uint64 // Go 1.27: typelink-flagged descriptors are [types+8, types+typedesclen); itabs at types+itaboffset
	Rodata                                        uint64
	Gofunc                                        uint64
	Epclntab                                      uint64
	Typelinks, Itablinks                          Slice
}

type Slice struct{ Ptr, Len, Cap uint64 }

func loadBin(path string) (*Bin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := &Bin{Path: path, File: raw, Sects: map[string]*Section{}}
	for _, s := range f.Sections {
		if s.Name == "" || s.Flags&elf.SHF_ALLOC == 0 {
			continue
		}
		sec := &Section{Name: s.Name, Addr: s.Addr, Off: s.Offset, Size: s.Size, NoBits: s.Type == elf.SHT_NOBITS}
		if !sec.NoBits {
			sec.Data = raw[s.Offset : s.Offset+s.Size]
		}
		b.Sects[s.Name] = sec
		b.Order = append(b.Order, sec)
	}
	sort.Slice(b.Order, func(i, j int) bool { return b.Order[i].Addr < b.Order[j].Addr })
	b.Text = b.Sects[".text"]
	if b.Text == nil {
		return nil, fmt.Errorf("%s: no .text", path)
	}
	pcs := b.Sects[".gopclntab"]
	if pcs == nil {
		return nil, fmt.Errorf("%s: no .gopclntab", path)
	}
	if bi, err := buildinfo.Read(bytes.NewReader(raw)); err == nil {
		b.GoVer = bi.GoVersion
	}
	if mod := b.Sects[".go.module"]; mod != nil {
		// Go 1.26 and 1.27 both keep moduledata in .go.module; 1.27 changed
		// the field layout (typedesclen/itaboffset/itabsize replace the
		// typelinks/itablinks slices) and moved type descriptors to .go.type.
		b.Layout = "go1.26"
		if b.Sects[".go.type"] != nil || strings.HasPrefix(b.GoVer, "go1.27") {
			b.Layout = "go1.27"
		}
		b.Mod = parseModuledata(mod.Data, b.Layout)
		if b.Mod.Epclntab != pcs.Addr+pcs.Size {
			return nil, fmt.Errorf("%s: moduledata.epclntab %#x != end of .gopclntab %#x (layout %s misparsed?)", path, b.Mod.Epclntab, pcs.Addr+pcs.Size, b.Layout)
		}
	} else {
		// Go <= 1.25: firstmoduledata is an ordinary symbol in .noptrdata.
		// Find it: pcHeader pointer == .gopclntab address, followed by the
		// funcnametab slice whose pointer is pcHeader + funcnameOffset.
		want := pcs.Addr + binary.LittleEndian.Uint64(pcs.Data[32:])
		found := false
		for _, sn := range []string{".noptrdata", ".data"} {
			ds := b.Sects[sn]
			if ds == nil {
				continue
			}
			for off := 0; off+400 <= len(ds.Data); off += 8 {
				if binary.LittleEndian.Uint64(ds.Data[off:]) == pcs.Addr && binary.LittleEndian.Uint64(ds.Data[off+8:]) == want {
					b.Mod = parseModuledata(ds.Data[off:], "go1.25")
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: no .go.module and moduledata not found in .noptrdata/.data", path)
		}
		b.Layout = "go1.25"
	}
	if b.Mod.PcHeader != pcs.Addr {
		return nil, fmt.Errorf("%s: moduledata.pcHeader %#x != .gopclntab addr %#x", path, b.Mod.PcHeader, pcs.Addr)
	}
	if b.Mod.Text < b.Text.Addr || b.Mod.Text >= b.Text.Addr+b.Text.Size {
		return nil, fmt.Errorf("%s: moduledata.text %#x outside .text [%#x,%#x)", path, b.Mod.Text, b.Text.Addr, b.Text.Addr+b.Text.Size)
	}
	p, err := parsePcln(pcs.Data, pcs.Addr, b.Mod)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	b.Pcln = p
	if !p.Inside {
		// Go 1.25: go:func.* and findfunctab are in .rodata. findfunctab's
		// size follows from the text range; go:func.* ends where findfunctab
		// (or the next known symbol) starts.
		ro := b.Sects[".rodata"]
		if ro == nil || b.Mod.Gofunc < ro.Addr || b.Mod.Gofunc >= ro.Addr+ro.Size || b.Mod.Findfunctab < ro.Addr || b.Mod.Findfunctab >= ro.Addr+ro.Size {
			return nil, fmt.Errorf("%s: go:func.*/findfunctab not in .rodata", path)
		}
		p.Gofunc = Range{b.Mod.Gofunc - ro.Addr, 0}
		end := ro.Addr + ro.Size
		for _, c := range []uint64{b.Mod.Findfunctab, b.Mod.Etypes, b.Mod.Typelinks.Ptr, b.Mod.Itablinks.Ptr} {
			if c > b.Mod.Gofunc && c < end {
				end = c
			}
		}
		p.Gofunc.Len = end - b.Mod.Gofunc
		p.Findfunctab = Range{b.Mod.Findfunctab - ro.Addr, 0}
	}
	b.Funcs = p.funcs(b.Mod.Text)
	if !p.Inside {
		p.Findfunctab.Len = uint64(len(genFindfunctab(b.Funcs)))
	}
	b.ByName = map[string][]*Func{}
	for _, fn := range b.Funcs {
		b.ByName[fn.Name] = append(b.ByName[fn.Name], fn)
	}
	return b, nil
}

func parseModuledata(d []byte, layout string) *Moduledata {
	u := func(off int) uint64 { return binary.LittleEndian.Uint64(d[off:]) }
	sl := func(off int) Slice { return Slice{u(off), u(off + 8), u(off + 16)} }
	m := &Moduledata{}
	m.PcHeader = u(0)
	m.Funcnametab = sl(8)
	m.Cutab = sl(32)
	m.Filetab = sl(56)
	m.Pctab = sl(80)
	m.Pclntable = sl(104)
	m.Ftab = sl(128)
	m.Findfunctab = u(152)
	m.Minpc, m.Maxpc = u(160), u(168)
	m.Text, m.Etext = u(176), u(184)
	m.Noptrdata, m.Enoptrdata = u(192), u(200)
	m.Data, m.Edata = u(208), u(216)
	m.Bss, m.Ebss = u(224), u(232)
	m.Noptrbss, m.Enoptrbss = u(240), u(248)
	// covctrs 256, ecovctrs 264, end 272, gcdata 280, gcbss 288
	m.Types = u(296)
	switch layout {
	case "go1.27":
		// runtime/symtab.go (1.27): types, typedesclen, etypes, itaboffset,
		// itabsize, rodata, gofunc, epclntab, textsectmap, ptab, ... (no
		// typelinks/itablinks: the runtime scans the descriptors instead).
		m.Typedesclen, m.Etypes = u(304), u(312)
		m.Itaboffset, m.Itabsize = u(320), u(328)
		m.Rodata, m.Gofunc, m.Epclntab = u(336), u(344), u(352)
		return m
	case "go1.26":
		m.Etypes = u(304)
		m.Rodata = u(312)
		m.Gofunc = u(320)
		m.Epclntab = u(328)
		m.Typelinks = sl(336 + 24)
		m.Itablinks = sl(336 + 48)
	default: // go1.25
		m.Etypes = u(304)
		m.Rodata = u(312)
		m.Gofunc = u(320)
		m.Typelinks = sl(328 + 24)
		m.Itablinks = sl(328 + 48)
	}
	return m
}

const pclnMagic120 = 0xfffffff1

func parsePcln(d []byte, addr uint64, m *Moduledata) (*Pcln, error) {
	if len(d) < 72 || binary.LittleEndian.Uint32(d) != pclnMagic120 {
		return nil, fmt.Errorf("pclntab magic %#x, want %#x (Go 1.20+ format)", binary.LittleEndian.Uint32(d), pclnMagic120)
	}
	u := func(off int) uint64 { return binary.LittleEndian.Uint64(d[off:]) }
	p := &Pcln{Data: d, Addr: addr, MinLC: d[6], PtrSize: d[7]}
	if p.PtrSize != 8 {
		return nil, fmt.Errorf("ptrSize %d unsupported", p.PtrSize)
	}
	p.NFunc = int(u(8))
	p.NFiles = int(u(16))
	p.FuncnameOff, p.CuOff, p.FiletabOff, p.PctabOff, p.FunctabOff = u(32), u(40), u(48), u(56), u(64)
	// Table bounds come from the pcHeader offsets (each table runs to the
	// next one, including any alignment padding); moduledata only confirms
	// the starts. (Go 1.25's moduledata.cutab length is in bytes, 1.26's in
	// elements, so the slice lengths are not used.)
	end := uint64(len(d))
	if m.Epclntab != 0 && m.Gofunc >= addr && m.Gofunc < addr+uint64(len(d)) {
		p.Inside = true
		p.Gofunc = Range{m.Gofunc - addr, m.Findfunctab - m.Gofunc}
		p.Findfunctab = Range{m.Findfunctab - addr, m.Epclntab - m.Findfunctab}
		end = p.Gofunc.Off
	}
	p.Funcnametab = Range{p.FuncnameOff, p.CuOff - p.FuncnameOff}
	p.Cutab = Range{p.CuOff, p.FiletabOff - p.CuOff}
	p.Filetab = Range{p.FiletabOff, p.PctabOff - p.FiletabOff}
	p.Pctab = Range{p.PctabOff, p.FunctabOff - p.PctabOff}
	p.Functab = Range{p.FunctabOff, end - p.FunctabOff}
	if m.Funcnametab.Ptr-addr != p.FuncnameOff || m.Cutab.Ptr-addr != p.CuOff || m.Filetab.Ptr-addr != p.FiletabOff || m.Pctab.Ptr-addr != p.PctabOff || m.Pclntable.Ptr-addr != p.FunctabOff {
		return nil, fmt.Errorf("pcHeader offsets disagree with moduledata")
	}
	if m.Ftab.Ptr != m.Pclntable.Ptr || m.Ftab.Len != uint64(p.NFunc)+1 {
		return nil, fmt.Errorf("ftab slice unexpected: %+v", m.Ftab)
	}
	return p, nil
}

// funcs decodes the functab into Func entries.
func (p *Pcln) funcs(text uint64) []*Func {
	ft := p.Data[p.Functab.Off:]
	out := make([]*Func, p.NFunc)
	for i := 0; i < p.NFunc; i++ {
		entryOff := binary.LittleEndian.Uint32(ft[8*i:])
		funcOff := binary.LittleEndian.Uint32(ft[8*i+4:])
		nextOff := binary.LittleEndian.Uint32(ft[8*(i+1):])
		nameOff := int32(binary.LittleEndian.Uint32(ft[funcOff+4:]))
		out[i] = &Func{Idx: i, Entry: text + uint64(entryOff), End: text + uint64(nextOff), FuncOff: funcOff, NameOff: nameOff, Name: p.name(nameOff)}
		if e2 := binary.LittleEndian.Uint32(ft[funcOff:]); e2 != entryOff {
			panic(fmt.Sprintf("func %d: functab entryoff %#x != _func.entryOff %#x", i, entryOff, e2))
		}
	}
	return out
}

func (p *Pcln) name(off int32) string {
	t := p.Data[p.Funcnametab.Off+uint64(off) : p.Funcnametab.End()]
	i := bytes.IndexByte(t, 0)
	return string(t[:i])
}

// funcRecordSize returns the size of the _func record (including pcdata and
// funcdata arrays) that starts at funcOff.
const funcSize = 11 * 4

func (p *Pcln) funcRecord(funcOff uint32) (npcdata, nfuncdata uint32, size uint32) {
	r := p.Data[p.Functab.Off+uint64(funcOff):]
	npcdata = binary.LittleEndian.Uint32(r[28:])
	nfuncdata = uint32(r[43])
	return npcdata, nfuncdata, funcSize + 4*npcdata + 4*nfuncdata
}

// sectionOf returns the allocated section containing addr, or nil.
func (b *Bin) sectionOf(addr uint64) *Section {
	i := sort.Search(len(b.Order), func(i int) bool { return b.Order[i].Addr+b.Order[i].Size > addr })
	if i < len(b.Order) && b.Order[i].Addr <= addr {
		return b.Order[i]
	}
	return nil
}

// funcAt returns the function containing addr (by entry <= addr < end).
func (b *Bin) funcAt(addr uint64) *Func {
	i := sort.Search(len(b.Funcs), func(i int) bool { return b.Funcs[i].End > addr })
	if i < len(b.Funcs) && b.Funcs[i].Entry <= addr {
		return b.Funcs[i]
	}
	return nil
}

func (b *Bin) funcBytes(f *Func) []byte {
	return b.Text.Data[f.Entry-b.Text.Addr : f.End-b.Text.Addr]
}
