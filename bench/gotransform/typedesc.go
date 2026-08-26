package main

import (
	"encoding/binary"
	"fmt"
)

// Type-descriptor offset rewriting (internal/abi/type.go, Go 1.27; the
// same descriptor layout is used by 1.26).
//
// Descriptors hold three kinds of 32-bit relative fields that the content
// map cannot fix because they are not absolute pointers: nameOff and typeOff
// (relative to moduledata.types) and textOff (relative to moduledata.text).
// The walker starts from the typelink-flagged descriptors (1.27: the range
// [types+8, types+typedesclen) walked with DescriptorSize; 1.26: the
// .typelink table) and the itabs, follows every *Type pointer and typeOff to
// reach the non-typelinked descriptors, and re-targets each field: nameOff/
// typeOff through the type section's content map, textOff through the
// function map. The rewritten values are written into the predicted section
// at the field's mapped position. Nothing is transmitted.

const (
	kindMask   = 0x1f
	kindArray  = 17
	kindChan   = 18
	kindFunc   = 19
	kindIface  = 20
	kindMap    = 21
	kindPtr    = 22
	kindSlice  = 23
	kindStruct = 25
	kindMax    = 26

	tflagUncommon = 1 << 0
	sizeType      = 48
	sizeUncommon  = 16
	sizeMethod    = 16
	sizeImethod   = 8
	sizeField     = 24
)

// baseSize returns sizeof(the kind's descriptor struct) on amd64. Go 1.27
// grew MapType by 24 bytes (KeysOff/KeyStride/ElemsOff/ElemStride replaced
// SlotSize); everything else is unchanged from 1.26.
func baseSize(kind byte, layout string) int {
	switch kind {
	case kindArray:
		return 72
	case kindChan:
		return 64
	case kindFunc:
		return 56
	case kindIface:
		return 80
	case kindMap:
		if layout == "go1.27" {
			return 136
		}
		return 112
	case kindPtr, kindSlice:
		return 56
	case kindStruct:
		return 80
	default:
		return sizeType
	}
}

var typeRewrite = true

type typeStats struct {
	Descs, Itabs, NameOff, TypeOff, TextOff, PkgPath, Unmapped, Bad int
	UnmappedName, UnmappedType, UnmappedText                        int
	UnmappedTextCls                                                 [rcNum]int
}

func (s typeStats) String() string {
	return fmt.Sprintf("descriptors=%d itabs=%d rewritten nameOff=%d typeOff=%d textOff=%d name.pkgPath=%d unmapped=%d (name %d, type %d, text %d: unmatched-fn %d outside %d) bad=%d", s.Descs, s.Itabs, s.NameOff, s.TypeOff, s.TextOff, s.PkgPath, s.Unmapped, s.UnmappedName, s.UnmappedType, s.UnmappedText, s.UnmappedTextCls[rcTextUnmatch], s.UnmappedTextCls[rcOutside], s.Bad)
}

// rewriteTypeOffsets rewrites the relative fields of old's descriptors into
// pred, the predicted new section named sect (the one holding
// moduledata.types) laid out through dm.
// If vote is non-nil it is called for every relative field with the field's
// old section offset and the old absolute target (kind 'n' nameOff, 't'
// typeOff, 'x' textOff); with pred == nil nothing is written.
func rewriteTypeOffsets(old, new *Bin, sect string, dm *DataMap, mp *mapper, pred []byte, vote func(off uint64, target uint64, kind byte)) typeStats {
	var st typeStats
	os, ns := old.Sects[sect], new.Sects[sect]
	if os == nil || ns == nil || dm == nil {
		return st
	}
	om, nm := old.Mod, new.Mod
	d := os.Data
	inSect := func(a uint64) bool { return a >= os.Addr && a < os.Addr+os.Size }
	u32 := func(off uint64) uint32 { return binary.LittleEndian.Uint32(d[off:]) }
	u64 := func(off uint64) uint64 { return binary.LittleEndian.Uint64(d[off:]) }
	// position of an old section offset in the predicted section
	place := func(off uint64) int64 {
		i := int(off) / dm.Block
		if i >= len(dm.Delta) {
			i = len(dm.Delta) - 1
		}
		return int64(off) + dm.Delta[i]
	}
	put32 := func(off uint64, v uint32) {
		if pred == nil {
			return
		}
		p := place(off)
		if p >= 0 && p+4 <= int64(len(pred)) {
			binary.LittleEndian.PutUint32(pred[p:], v)
		}
	}
	// nameOff/typeOff: old types+off -> new types+off'. Targets are usually
	// in the type section (through its content map) but name data can sit in
	// other sections (.noptrdata/.rodata), which mapAddr handles the same way.
	mapTypesOff := func(off uint32) (uint32, bool) {
		a := om.Types + uint64(off)
		nv, cls := mp.mapAddr(a, nil)
		if cls == rcOutside || cls == rcTextUnmatch {
			return off, false
		}
		return uint32(nv - nm.Types), true
	}
	mapTextOff := func(off uint32) (uint32, bool) {
		a := om.Text + uint64(off)
		nv, cls := mp.mapAddr(a, nil)
		if cls != rcTextSelf && cls != rcTextMatched && cls != rcTextNone {
			st.UnmappedTextCls[cls]++
			return off, false
		}
		return uint32(nv - nm.Text), true
	}
	doName := func(off uint64) {
		if vote != nil {
			vote(off, om.Types+uint64(u32(off)), 'n')
		}
		if v, ok := mapTypesOff(u32(off)); ok {
			put32(off, v)
			st.NameOff++
		} else {
			st.Unmapped++
			st.UnmappedName++
		}
	}
	// a name's trailing pkgPath nameOff (flag 1<<2), at the name's old address
	doNameData := func(a uint64) {
		if !inSect(a) {
			return
		}
		o := a - os.Addr
		if o+2 > uint64(len(d)) {
			return
		}
		flag := d[o]
		if flag&(1<<2) == 0 {
			return
		}
		p := o + 1
		var n uint64
		for shift := 0; ; shift += 7 {
			if p >= uint64(len(d)) {
				return
			}
			x := d[p]
			p++
			n |= uint64(x&0x7f) << shift
			if x&0x80 == 0 {
				break
			}
		}
		p += n
		if flag&(1<<1) != 0 { // tag
			var t uint64
			for shift := 0; ; shift += 7 {
				if p >= uint64(len(d)) {
					return
				}
				x := d[p]
				p++
				t |= uint64(x&0x7f) << shift
				if x&0x80 == 0 {
					break
				}
			}
			p += t
		}
		if p+4 > uint64(len(d)) {
			return
		}
		if vote != nil {
			vote(p, om.Types+uint64(u32(p)), 'n')
		}
		if v, ok := mapTypesOff(u32(p)); ok {
			put32(p, v)
			st.PkgPath++
		}
	}
	doNameOff := func(off uint64) {
		doNameData(om.Types + uint64(u32(off)))
		doName(off)
	}
	var queue []uint64
	visited := map[uint64]bool{}
	enqueue := func(a uint64) {
		if a == 0 || !inSect(a) || visited[a] {
			return
		}
		visited[a] = true
		queue = append(queue, a)
	}
	doType := func(off uint64) {
		v := u32(off)
		if v == 0 {
			return
		}
		if vote != nil {
			vote(off, om.Types+uint64(v), 't')
		}
		enqueue(om.Types + uint64(v))
		if nv, ok := mapTypesOff(v); ok {
			put32(off, nv)
			st.TypeOff++
		} else {
			st.Unmapped++
			st.UnmappedType++
		}
	}
	doText := func(off uint64) {
		v := u32(off)
		if v == ^uint32(0) {
			return
		}
		if vote != nil {
			vote(off, om.Text+uint64(v), 'x')
		}
		if nv, ok := mapTextOff(v); ok {
			put32(off, nv)
			st.TextOff++
		} else {
			st.Unmapped++
			st.UnmappedText++
		}
	}
	// roots
	switch {
	case om.Typedesclen > 0: // Go 1.27
		a := om.Types + 8
		end := om.Types + om.Typedesclen
		for a < end && inSect(a) {
			a = (a + 7) &^ 7
			o := a - os.Addr
			if o+sizeType > uint64(len(d)) {
				break
			}
			enqueue(a)
			sz := descSize(d, o, old.Layout)
			if sz <= 0 {
				st.Bad++
				break
			}
			a += uint64(sz)
		}
		// itabs: {Inter *InterfaceType, Type *Type, Hash, Fun [n]uintptr}
		a = om.Types + om.Itaboffset
		end = a + om.Itabsize
		for a+24 <= end && inSect(a) {
			o := a - os.Addr
			inter, typ := u64(o), u64(o+8)
			enqueue(inter)
			enqueue(typ)
			st.Itabs++
			n := 0
			if inSect(inter) {
				n = int(u64(inter - os.Addr + 64)) // len(Methods)
			}
			a += 24 + 8*uint64(n)
			if n < 0 || n > 1<<16 {
				break
			}
		}
	default: // Go 1.26: .typelink / .itablink
		if tl := old.Sects[".typelink"]; tl != nil {
			for i := 0; i+4 <= len(tl.Data); i += 4 {
				enqueue(om.Types + uint64(binary.LittleEndian.Uint32(tl.Data[i:])))
			}
		}
		if il := old.Sects[".itablink"]; il != nil {
			for i := 0; i+8 <= len(il.Data); i += 8 {
				a := binary.LittleEndian.Uint64(il.Data[i:])
				if inSect(a) && a+16 <= os.Addr+os.Size {
					enqueue(u64(a - os.Addr))
					enqueue(u64(a - os.Addr + 8))
					st.Itabs++
				}
			}
		}
	}
	for len(queue) > 0 {
		a := queue[0]
		queue = queue[1:]
		o := a - os.Addr
		if o+sizeType > uint64(len(d)) {
			st.Bad++
			continue
		}
		kind := d[o+23] & kindMask
		if kind == 0 || kind > kindMax {
			st.Bad++
			continue
		}
		st.Descs++
		tflag := d[o+20]
		doNameOff(o + 40) // Str
		doType(o + 44)    // PtrToThis
		base := uint64(baseSize(kind, old.Layout))
		ut := uint64(0)
		if tflag&tflagUncommon != 0 {
			ut = o + base
			if ut+sizeUncommon <= uint64(len(d)) {
				doNameOff(ut) // PkgPath
			}
		}
		addStart := o + base
		if ut != 0 {
			addStart += sizeUncommon
		}
		switch kind {
		case kindArray:
			enqueue(u64(o + 48))
			enqueue(u64(o + 56))
		case kindChan, kindPtr, kindSlice:
			enqueue(u64(o + 48))
		case kindMap:
			enqueue(u64(o + 48))
			enqueue(u64(o + 56))
			enqueue(u64(o + 64))
		case kindFunc:
			in := int(binary.LittleEndian.Uint16(d[o+48:]))
			out := int(binary.LittleEndian.Uint16(d[o+50:]) & 0x7fff)
			for k := 0; k < in+out; k++ {
				p := addStart + 8*uint64(k)
				if p+8 <= uint64(len(d)) {
					enqueue(u64(p))
				}
			}
		case kindIface:
			doNameData(u64(o + 48)) // PkgPath Name
			ms, n := u64(o+56), u64(o+64)
			if inSect(ms) && n < 1<<16 {
				mo := ms - os.Addr
				for k := uint64(0); k < n && mo+sizeImethod*(k+1) <= uint64(len(d)); k++ {
					doNameOff(mo + sizeImethod*k)
					doType(mo + sizeImethod*k + 4)
				}
			}
		case kindStruct:
			doNameData(u64(o + 48))
			fs, n := u64(o+56), u64(o+64)
			if inSect(fs) && n < 1<<16 {
				fo := fs - os.Addr
				for k := uint64(0); k < n && fo+sizeField*(k+1) <= uint64(len(d)); k++ {
					doNameData(u64(fo + sizeField*k))
					enqueue(u64(fo + sizeField*k + 8))
				}
			}
		}
		if ut != 0 && ut+sizeUncommon <= uint64(len(d)) {
			mcount := uint64(binary.LittleEndian.Uint16(d[ut+4:]))
			moff := uint64(u32(ut + 8))
			for k := uint64(0); k < mcount; k++ {
				mo := ut + moff + sizeMethod*k
				if mo+sizeMethod > uint64(len(d)) {
					break
				}
				doNameOff(mo)
				doType(mo + 4)
				doText(mo + 8)
				doText(mo + 12)
			}
		}
	}
	return st
}

// descSize is abi.Type.DescriptorSize for the descriptor at section offset o.
func descSize(d []byte, o uint64, layout string) int {
	kind := d[o+23] & kindMask
	if kind == 0 || kind > kindMax {
		return -1
	}
	tflag := d[o+20]
	size := baseSize(kind, layout)
	mcount := 0
	if tflag&tflagUncommon != 0 {
		ut := o + uint64(size)
		if ut+sizeUncommon > uint64(len(d)) {
			return -1
		}
		mcount = int(binary.LittleEndian.Uint16(d[ut+4:]))
		size += sizeUncommon
	}
	switch kind {
	case kindFunc:
		in := int(binary.LittleEndian.Uint16(d[o+48:]))
		out := int(binary.LittleEndian.Uint16(d[o+50:]) & 0x7fff)
		size += (in + out) * 8
	case kindIface:
		size += int(binary.LittleEndian.Uint64(d[o+64:])) * sizeImethod
	case kindStruct:
		size += int(binary.LittleEndian.Uint64(d[o+64:])) * sizeField
	}
	return size + mcount*sizeMethod
}
