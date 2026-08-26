package gobin

import "encoding/binary"

// findfunctab bucket geometry, from cmd/link/internal/ld/pcln.go.
const (
	subbucketSize = 256
	bucketSize    = 4096
	subbuckets    = bucketSize / subbucketSize
)

// GenFindfunctab regenerates runtime.findfunctab from the function table.
// The linker derives it from nothing but the function boundaries, so the
// decoder can rebuild it exactly and it is never transmitted.
func GenFindfunctab(funcs []*Func) []byte {
	lo := funcs[0].Entry
	hi := funcs[len(funcs)-1].End
	n := int((hi - lo + subbucketSize - 1) / subbucketSize)
	nb := int((hi - lo + bucketSize - 1) / bucketSize)
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
		for p := f.Entry; p < f.End; p += subbucketSize {
			set(int((p-lo)/subbucketSize), int32(fi))
		}
		set(int((f.End-1-lo)/subbucketSize), int32(fi))
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

// ModalShape is the most common (npcdata, nfuncdata) pair among b's _func
// records. It is what the decoder assumes for a function that is new in the
// release, so only the exceptions have to be transmitted.
func ModalShape(b *Bin) [2]uint32 {
	count := map[[2]uint32]int{}
	for _, f := range b.Funcs {
		np, nf, _ := b.Pcln.Record(f.FuncOff)
		count[[2]uint32{np, nf}]++
	}
	var best [2]uint32
	bestN := -1
	for k, c := range count {
		// ties broken by the key so that the choice does not depend on map
		// iteration order: encoder and decoder must agree exactly.
		if c > bestN || c == bestN && (k[0] < best[0] || k[0] == best[0] && k[1] < best[1]) {
			best, bestN = k, c
		}
	}
	return best
}

// PcTableLen returns the length of the self-delimiting pc-value table at
// off: (value delta, pc delta) varint pairs terminated by a zero value
// delta, which is only legal in the first pair.
func PcTableLen(tab []byte, off int) int {
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
