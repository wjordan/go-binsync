// Package gobin parses the parts of a Go ELF binary the codec predicts from:
// the section table, the pclntab with its function table, the moduledata, and
// the type descriptors.
//
// It supports exactly one Go release at a time -- the current stable one, see
// SupportedGo. Each minor release moves this ground: 1.25 changed entryOff
// handling, 1.26 moved moduledata into .go.module, 1.27 moved type descriptors
// and itabs into a sorted .go.type section, dropped .typelink/.itablink, grew
// MapType by 24 bytes and made go:func.* alignment data-dependent. Supporting
// a matrix of those is how a codec becomes untestable; supporting one, gated
// by a byte-exact self-prediction check, is how it stays reviewable
// (docs/DESIGN.md 3.6, D14). Anything else takes the plain codec.
package gobin

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// SupportedGo is the toolchain prefix the Go-aware codec understands.
const SupportedGo = "go1.27"

// PclnMagic is the pcHeader magic of the Go 1.20+ pclntab format.
const PclnMagic = 0xfffffff1

// Section is one allocated ELF section with its bytes. Data is nil for
// NOBITS sections and throughout a Skeleton, which carries a section table
// but no content.
type Section struct {
	Name   string
	Addr   uint64
	Off    uint64
	Size   uint64
	Data   []byte
	NoBits bool
}

// Range is a (offset, length) pair inside a section.
type Range struct{ Off, Len uint64 }

// End is the offset one past the range.
func (r Range) End() uint64 { return r.Off + r.Len }

// Func is one entry of the pclntab function table.
type Func struct {
	Idx     int
	Name    string
	Entry   uint64 // absolute address of the function's first instruction
	End     uint64 // the next function's entry (or the end pc, for the last)
	FuncOff uint32 // offset of the _func record from the start of functab
	NameOff int32  // offset of the name in funcnametab
}

// Size is the number of bytes the function occupies, padding included.
func (f *Func) Size() uint64 { return f.End - f.Entry }

// Slice is a Go slice header as it appears in moduledata.
type Slice struct{ Ptr, Len, Cap uint64 }

// Moduledata is the subset of runtime.firstmoduledata the predictors read.
// Field offsets are those of the supported release's runtime/symtab.go.
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
	// Typelink-flagged descriptors occupy [Types+8, Types+Typedesclen);
	// itabs occupy [Types+Itaboffset, +Itabsize).
	Typedesclen, Itaboffset, Itabsize uint64
	Rodata, Gofunc, Epclntab          uint64
}

// Pcln is a parsed pclntab. Every offset is relative to the start of the
// .gopclntab section.
type Pcln struct {
	Data    []byte
	Addr    uint64
	NFunc   int
	NFiles  int
	MinLC   uint8
	PtrSize uint8

	// Table starts as the pcHeader records them.
	FuncnameOff, CuOff, FiletabOff, PctabOff, FunctabOff uint64

	// Each table runs to the start of the next, so these bounds include
	// whatever alignment padding the linker left.
	Funcnametab, Cutab, Filetab, Pctab, Functab, Gofunc, Findfunctab Range
}

// Bin is a parsed Go ELF binary.
type Bin struct {
	File   []byte
	Sects  map[string]*Section
	Order  []*Section // allocated sections, by address
	Text   *Section
	Pcln   *Pcln
	Mod    *Moduledata
	Funcs  []*Func // in functab order, which is address order
	GoVer  string  // from .go.buildinfo, e.g. "go1.27.0"
	Module string  // main module path, for logs
	Vers   string  // main module version or vcs.revision, for logs
}

// Unsupported marks an input the Go-aware codec declines. It is never a
// failure: the caller falls back to the plain codec.
type Unsupported struct{ Reason string }

func (e *Unsupported) Error() string { return e.Reason }

func unsup(f string, a ...any) error { return &Unsupported{fmt.Sprintf(f, a...)} }

// Parse reads a Go ELF binary. It returns *Unsupported for anything the
// Go-aware codec cannot predict: a non-ELF file, a foreign architecture, a
// PIE, a binary from another toolchain, or a layout it does not recognise.
func Parse(raw []byte) (*Bin, error) {
	f, err := elf.NewFile(bytes.NewReader(raw))
	if err != nil {
		return nil, unsup("not an ELF file: %v", err)
	}
	if f.Machine != elf.EM_X86_64 || f.Class != elf.ELFCLASS64 || f.ByteOrder != binary.LittleEndian {
		return nil, unsup("not linux/amd64 (%v, %v)", f.Machine, f.Class)
	}
	if f.Type != elf.ET_EXEC {
		return nil, unsup("not a static executable (%v); PIE and shared objects relocate at load time", f.Type)
	}
	b := &Bin{File: raw, Sects: map[string]*Section{}}
	for _, s := range f.Sections {
		if s.Name == "" || s.Flags&elf.SHF_ALLOC == 0 {
			continue
		}
		sec := &Section{Name: s.Name, Addr: s.Addr, Off: s.Offset, Size: s.Size, NoBits: s.Type == elf.SHT_NOBITS}
		if !sec.NoBits {
			if s.Offset+s.Size > uint64(len(raw)) {
				return nil, unsup("section %s runs past the end of the file", s.Name)
			}
			sec.Data = raw[s.Offset : s.Offset+s.Size]
		}
		b.Sects[s.Name] = sec
		b.Order = append(b.Order, sec)
	}
	sort.Slice(b.Order, func(i, j int) bool { return b.Order[i].Addr < b.Order[j].Addr })
	if b.Text = b.Sects[".text"]; b.Text == nil {
		return nil, unsup("no .text section")
	}
	pcs := b.Sects[".gopclntab"]
	if pcs == nil {
		return nil, unsup("no .gopclntab: not a Go binary, or built without it")
	}
	if bi, err := buildinfo.Read(bytes.NewReader(raw)); err == nil {
		b.GoVer, b.Module = bi.GoVersion, bi.Path
		b.Vers = bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && b.Vers == "" || s.Key == "vcs.revision" && b.Vers == "(devel)" {
				b.Vers = s.Value
			}
		}
	}
	if !strings.HasPrefix(b.GoVer, SupportedGo) {
		return nil, unsup("built with %q, the Go-aware codec supports %s", b.GoVer, SupportedGo)
	}
	mod := b.Sects[".go.module"]
	if mod == nil || b.Sects[".go.type"] == nil {
		return nil, unsup("no .go.module/.go.type: not the %s layout", SupportedGo)
	}
	if b.Mod, err = parseModuledata(mod.Data); err != nil {
		return nil, err
	}
	if b.Mod.PcHeader != pcs.Addr {
		return nil, unsup("moduledata.pcHeader %#x is not .gopclntab %#x", b.Mod.PcHeader, pcs.Addr)
	}
	if b.Mod.Epclntab != pcs.Addr+pcs.Size {
		return nil, unsup("moduledata.epclntab %#x is not the end of .gopclntab %#x", b.Mod.Epclntab, pcs.Addr+pcs.Size)
	}
	if b.Mod.Text < b.Text.Addr || b.Mod.Text >= b.Text.Addr+b.Text.Size {
		return nil, unsup("moduledata.text %#x outside .text", b.Mod.Text)
	}
	if b.Pcln, err = parsePcln(pcs.Data, pcs.Addr, b.Mod); err != nil {
		return nil, err
	}
	if b.Funcs, err = b.Pcln.funcs(b.Mod.Text); err != nil {
		return nil, err
	}
	return b, nil
}

func parseModuledata(d []byte) (*Moduledata, error) {
	// Field offsets of runtime.moduledata for the supported release; the
	// order is: pcHeader, funcnametab, cutab, filetab, pctab, pclntable,
	// ftab, findfunctab, min/maxpc, text..enoptrbss, covctrs, ecovctrs,
	// end, gcdata, gcbss, types, typedesclen, etypes, itaboffset, itabsize,
	// rodata, gofunc, epclntab, ...
	const need = 360
	if len(d) < need {
		return nil, unsup(".go.module is %d bytes, want at least %d", len(d), need)
	}
	u := func(off int) uint64 { return binary.LittleEndian.Uint64(d[off:]) }
	sl := func(off int) Slice { return Slice{u(off), u(off + 8), u(off + 16)} }
	m := &Moduledata{
		PcHeader:    u(0),
		Funcnametab: sl(8), Cutab: sl(32), Filetab: sl(56), Pctab: sl(80),
		Pclntable: sl(104), Ftab: sl(128), Findfunctab: u(152),
		Minpc: u(160), Maxpc: u(168),
		Text: u(176), Etext: u(184),
		Noptrdata: u(192), Enoptrdata: u(200),
		Data: u(208), Edata: u(216),
		Bss: u(224), Ebss: u(232),
		Noptrbss: u(240), Enoptrbss: u(248),
		Types: u(296), Typedesclen: u(304), Etypes: u(312),
		Itaboffset: u(320), Itabsize: u(328),
		Rodata: u(336), Gofunc: u(344), Epclntab: u(352),
	}
	return m, nil
}

func parsePcln(d []byte, addr uint64, m *Moduledata) (*Pcln, error) {
	if len(d) < 72 || binary.LittleEndian.Uint32(d) != PclnMagic {
		return nil, unsup("pclntab magic %#x, want %#x", binary.LittleEndian.Uint32(d), PclnMagic)
	}
	u := func(off int) uint64 { return binary.LittleEndian.Uint64(d[off:]) }
	p := &Pcln{Data: d, Addr: addr, MinLC: d[6], PtrSize: d[7]}
	if p.PtrSize != 8 {
		return nil, unsup("pclntab ptrSize %d", p.PtrSize)
	}
	p.NFunc, p.NFiles = int(u(8)), int(u(16))
	p.FuncnameOff, p.CuOff, p.FiletabOff, p.PctabOff, p.FunctabOff = u(32), u(40), u(48), u(56), u(64)
	if m.Gofunc < addr || m.Gofunc >= addr+uint64(len(d)) || m.Findfunctab < m.Gofunc {
		return nil, unsup("go:func.*/findfunctab are not inside .gopclntab")
	}
	// Each table runs to the start of the next, so the ranges carry the
	// linker's alignment padding with them and are length-exact.
	p.Gofunc = Range{m.Gofunc - addr, m.Findfunctab - m.Gofunc}
	p.Findfunctab = Range{m.Findfunctab - addr, m.Epclntab - m.Findfunctab}
	p.Funcnametab = Range{p.FuncnameOff, p.CuOff - p.FuncnameOff}
	p.Cutab = Range{p.CuOff, p.FiletabOff - p.CuOff}
	p.Filetab = Range{p.FiletabOff, p.PctabOff - p.FiletabOff}
	p.Pctab = Range{p.PctabOff, p.FunctabOff - p.PctabOff}
	p.Functab = Range{p.FunctabOff, p.Gofunc.Off - p.FunctabOff}
	for _, r := range []Range{p.Funcnametab, p.Cutab, p.Filetab, p.Pctab, p.Functab, p.Gofunc, p.Findfunctab} {
		if r.Off > r.End() || r.End() > uint64(len(d)) {
			return nil, unsup("pclntab table %+v outside the %d-byte section", r, len(d))
		}
	}
	if m.Funcnametab.Ptr-addr != p.FuncnameOff || m.Cutab.Ptr-addr != p.CuOff ||
		m.Filetab.Ptr-addr != p.FiletabOff || m.Pctab.Ptr-addr != p.PctabOff ||
		m.Pclntable.Ptr-addr != p.FunctabOff {
		return nil, unsup("pcHeader offsets disagree with moduledata")
	}
	if m.Ftab.Ptr != m.Pclntable.Ptr || m.Ftab.Len != uint64(p.NFunc)+1 {
		return nil, unsup("ftab slice %+v does not describe %d functions", m.Ftab, p.NFunc)
	}
	return p, nil
}

// FuncSize is the size of a _func record before its pcdata and funcdata
// arrays (internal/abi.FuncSize).
const FuncSize = 11 * 4

// Record returns the shape and total size of the _func record at funcOff.
func (p *Pcln) Record(funcOff uint32) (npcdata, nfuncdata, size uint32) {
	r := p.Data[p.Functab.Off+uint64(funcOff):]
	npcdata = binary.LittleEndian.Uint32(r[28:])
	nfuncdata = uint32(r[43])
	return npcdata, nfuncdata, FuncSize + 4*npcdata + 4*nfuncdata
}

// Table returns the bytes of one pclntab sub-table.
func (p *Pcln) Table(r Range) []byte { return p.Data[r.Off:r.End()] }

func (p *Pcln) funcs(text uint64) ([]*Func, error) {
	ft := p.Data[p.Functab.Off:p.Functab.End()]
	if uint64(len(ft)) < uint64(p.NFunc+1)*8 {
		return nil, unsup("functab holds %d bytes, too small for %d functions", len(ft), p.NFunc)
	}
	out := make([]*Func, p.NFunc)
	for i := range out {
		entryOff := binary.LittleEndian.Uint32(ft[8*i:])
		funcOff := binary.LittleEndian.Uint32(ft[8*i+4:])
		nextOff := binary.LittleEndian.Uint32(ft[8*(i+1):])
		if uint64(funcOff)+FuncSize > uint64(len(ft)) || nextOff < entryOff {
			return nil, unsup("functab entry %d is out of range", i)
		}
		if e2 := binary.LittleEndian.Uint32(ft[funcOff:]); e2 != entryOff {
			return nil, unsup("function %d: functab entryOff %#x != _func.entryOff %#x", i, entryOff, e2)
		}
		nameOff := int32(binary.LittleEndian.Uint32(ft[funcOff+4:]))
		name, err := p.Name(nameOff)
		if err != nil {
			return nil, err
		}
		out[i] = &Func{Idx: i, Name: name, Entry: text + uint64(entryOff), End: text + uint64(nextOff),
			FuncOff: funcOff, NameOff: nameOff}
	}
	return out, nil
}

// Name returns the function name at off in funcnametab.
func (p *Pcln) Name(off int32) (string, error) {
	if off < 0 || uint64(off) >= p.Funcnametab.Len {
		return "", unsup("name offset %d outside funcnametab", off)
	}
	t := p.Data[p.Funcnametab.Off+uint64(off) : p.Funcnametab.End()]
	i := bytes.IndexByte(t, 0)
	if i < 0 {
		return "", unsup("unterminated name at %d", off)
	}
	return string(t[:i]), nil
}

// SectionOf returns the allocated section containing addr, or nil.
func (b *Bin) SectionOf(addr uint64) *Section {
	i := sort.Search(len(b.Order), func(i int) bool { return b.Order[i].Addr+b.Order[i].Size > addr })
	if i < len(b.Order) && b.Order[i].Addr <= addr {
		return b.Order[i]
	}
	return nil
}

// FuncAt returns the function containing addr, or nil.
func (b *Bin) FuncAt(addr uint64) *Func {
	i := sort.Search(len(b.Funcs), func(i int) bool { return b.Funcs[i].End > addr })
	if i < len(b.Funcs) && b.Funcs[i].Entry <= addr {
		return b.Funcs[i]
	}
	return nil
}

// FuncBytes returns a function's machine code.
func (b *Bin) FuncBytes(f *Func) []byte {
	return b.Text.Data[f.Entry-b.Text.Addr : f.End-b.Text.Addr]
}

// ImageRange is the [lowest, highest) virtual address of the loaded image,
// which is what tells an absolute pointer from an ordinary integer.
func (b *Bin) ImageRange() (lo, hi uint64) {
	lo, hi = ^uint64(0), 0
	for _, s := range b.Order {
		if s.Addr == 0 {
			continue
		}
		lo = min(lo, s.Addr)
		hi = max(hi, s.Addr+s.Size)
	}
	return lo, hi
}

// Gofunc returns the go:func.* blob.
func (b *Bin) Gofunc() []byte { return b.Pcln.Table(b.Pcln.Gofunc) }

// Findfunctab returns the runtime's pc lookup table.
func (b *Bin) Findfunctab() []byte { return b.Pcln.Table(b.Pcln.Findfunctab) }
