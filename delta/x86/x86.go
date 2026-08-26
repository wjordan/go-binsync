// Package x86 relocates the PC-relative operands of amd64 machine code.
//
// A Go function that moves keeps its bytes, but every rip-relative operand
// and every rel32 branch that crosses the move has to be re-targeted. That
// is the whole of what this package does: decode, find the displacement
// field, ask the caller where the old target went, and write the new
// displacement back.
package x86

import (
	"encoding/binary"
	"hash/fnv"

	"golang.org/x/arch/x86/x86asm"
)

// Target is what the caller knows about an old address.
type Target struct {
	// Addr is where the old target landed in the new binary.
	Addr uint64
	// Known is false when the old target has no counterpart -- a function
	// that was deleted, or an address outside the image. The displacement is
	// then left alone and the correction pays for it.
	Known bool
}

// Stats counts what one relocation pass saw.
type Stats struct {
	Insns int
	// Fails counts bytes the decoder could not make sense of. They are left
	// as they are: the correction fixes them, and an instruction set this
	// package does not know costs bytes, never correctness.
	Fails   int
	Refs    int
	Unknown int
	NoFit   int
}

// Add folds s2 into s.
func (s *Stats) Add(s2 Stats) {
	s.Insns += s2.Insns
	s.Fails += s2.Fails
	s.Refs += s2.Refs
	s.Unknown += s2.Unknown
	s.NoFit += s2.NoFit
}

// Ref is one relocated displacement field, for the encoder's diagnostics.
type Ref struct {
	Off int // offset of the field within the code
	N   int // field width in bytes
}

// Relocate copies code (which lives at srcPC in the old binary) into out
// (which lives at dstPC in the new one) and re-targets every PC-relative
// operand through lookup. out is filled to its full length: short code is
// padded with INT3, exactly as the linker pads a function to its alignment.
//
// If refs is non-nil, every relocated field is appended to it.
func Relocate(code, out []byte, srcPC, dstPC uint64, lookup func(target uint64) Target, st *Stats, refs *[]Ref) {
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
			relocOne(code, out, i, inst, srcPC, dstPC, lookup, st)
			if refs != nil {
				*refs = append(*refs, Ref{i + inst.PCRelOff, inst.PCRel})
			}
		}
		i += inst.Len
	}
}

func relocOne(code, out []byte, i int, inst x86asm.Inst, srcPC, dstPC uint64, lookup func(uint64) Target, st *Stats) {
	fld := code[i+inst.PCRelOff : i+inst.PCRelOff+inst.PCRel]
	var disp int64
	switch inst.PCRel {
	case 1:
		disp = int64(int8(fld[0]))
	case 2:
		disp = int64(int16(binary.LittleEndian.Uint16(fld)))
	case 4:
		disp = int64(int32(binary.LittleEndian.Uint32(fld)))
	default:
		return
	}
	st.Refs++
	next := int64(inst.Len)
	target := uint64(int64(srcPC) + int64(i) + next + disp)
	t := lookup(target)
	if !t.Known {
		st.Unknown++
		return
	}
	nd := int64(t.Addr) - (int64(dstPC) + int64(i) + next)
	switch inst.PCRel {
	case 1:
		if nd < -128 || nd > 127 {
			st.NoFit++
			return
		}
		out[i+inst.PCRelOff] = byte(int8(nd))
	case 2:
		if nd < -32768 || nd > 32767 {
			st.NoFit++
			return
		}
		binary.LittleEndian.PutUint16(out[i+inst.PCRelOff:], uint16(int16(nd)))
	case 4:
		if nd < -1<<31 || nd >= 1<<31 {
			st.NoFit++
			return
		}
		binary.LittleEndian.PutUint32(out[i+inst.PCRelOff:], uint32(int32(nd)))
	}
}

// ContentHash hashes machine code with every PC-relative field zeroed, so
// that a function whose only change is where its targets moved hashes the
// same in both releases. That is what makes "did this function actually
// change?" answerable without symbols.
func ContentHash(code []byte) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(len(code)))
	h.Write(buf[:])
	var zero [8]byte
	for i := 0; i < len(code); {
		inst, err := x86asm.Decode(code[i:], 64)
		if err != nil || inst.Len == 0 {
			h.Write(code[i : i+1])
			i++
			continue
		}
		if inst.PCRel > 0 && i+inst.PCRelOff+inst.PCRel <= len(code) {
			h.Write(code[i : i+inst.PCRelOff])
			h.Write(zero[:inst.PCRel])
			h.Write(code[i+inst.PCRelOff+inst.PCRel : i+inst.Len])
		} else {
			h.Write(code[i:min(i+inst.Len, len(code))])
		}
		i += inst.Len
	}
	return h.Sum64()
}

// Equal reports whether two function bodies differ only in their
// PC-relative displacements. It is ContentHash without the hash: one decode
// pass, comparing the non-displacement bytes directly.
func Equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k := 0; k < len(a); {
		inst, err := x86asm.Decode(a[k:], 64)
		if err != nil || inst.Len == 0 {
			if a[k] != b[k] {
				return false
			}
			k++
			continue
		}
		end := min(k+inst.Len, len(a))
		if inst.PCRel > 0 && k+inst.PCRelOff+inst.PCRel <= len(a) {
			lo, hi := k+inst.PCRelOff, k+inst.PCRelOff+inst.PCRel
			if string(a[k:lo]) != string(b[k:lo]) || string(a[hi:end]) != string(b[hi:end]) {
				return false
			}
		} else if string(a[k:end]) != string(b[k:end]) {
			return false
		}
		k = end
	}
	return true
}
