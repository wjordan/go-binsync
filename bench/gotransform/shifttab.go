package main

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"

	"golang.org/x/arch/x86/x86asm"
)

// ShiftTable is a piecewise-constant old->new offset map for a section
// without content (.bss/.noptrbss). The encoder derives it from the
// references of unchanged functions (it sees both binaries) and transmits it;
// it is tiny because bss symbols are size-sorted and an insertion shifts
// everything after it by one constant.
type ShiftTable struct {
	Offs    []uint64
	Deltas  []int64
	Samples int
}

func (t *ShiftTable) Map(off uint64) uint64 {
	if t == nil || len(t.Offs) == 0 {
		return off
	}
	i := sort.Search(len(t.Offs), func(i int) bool { return t.Offs[i] > off }) - 1
	if i < 0 {
		return off
	}
	return uint64(int64(off) + t.Deltas[i])
}

// Encode serialises the breakpoints as varints (offset gaps, zigzag delta gaps).
func (t *ShiftTable) Encode() []byte {
	var out []byte
	var tmp [binary.MaxVarintLen64]byte
	var po uint64
	var pd int64
	for i := range t.Offs {
		n := binary.PutUvarint(tmp[:], t.Offs[i]-po)
		out = append(out, tmp[:n]...)
		n = binary.PutVarint(tmp[:], t.Deltas[i]-pd)
		out = append(out, tmp[:n]...)
		po, pd = t.Offs[i], t.Deltas[i]
	}
	return out
}

func (t *ShiftTable) String() string {
	if t == nil {
		return "nil"
	}
	ex := ""
	for i := 0; i < len(t.Offs) && i < 6; i++ {
		ex += fmt.Sprintf(" @%d:%+d", t.Offs[i], t.Deltas[i])
	}
	return fmt.Sprintf("%d breakpoints from %d samples, %d B raw:%s", len(t.Offs), t.Samples, len(t.Encode()), ex)
}

// deriveShiftTables walks matched functions whose content is unchanged and
// compares the targets of their PC-relative references in old and new for
// the given sections. One x86 decode pass per function (the non-displacement
// bytes are compared directly instead of hashing both sides), functions are
// processed in parallel.
func deriveShiftTables(old, new *Bin, m *Match, sections []string) map[string]*ShiftTable {
	type sample struct {
		off   uint64
		delta int64
	}
	secOf := func(b *Bin, t uint64) *Section {
		s := b.sectionOf(t)
		if s == nil && t > 0 {
			s = b.sectionOf(t - 1)
		}
		return s
	}
	want := map[string]bool{}
	for _, n := range sections {
		want[n] = true
	}
	const workers = 24
	perWorker := make([]map[string][]sample, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			samples := map[string][]sample{}
			perWorker[w] = samples
			var local []sample
			var localSect []string
			for j := w; j < len(new.Funcs); j += workers {
				g := new.Funcs[j]
				i := m.NewToOld[j]
				if i < 0 {
					continue
				}
				f := old.Funcs[i]
				if f.Size() != g.Size() {
					continue
				}
				oc, nc := old.funcBytes(f), new.funcBytes(g)
				local, localSect = local[:0], localSect[:0]
				same := true
				for k := 0; k < len(oc) && same; {
					inst, err := x86asm.Decode(oc[k:], 64)
					if err != nil || inst.Len == 0 {
						same = oc[k] == nc[k]
						k++
						continue
					}
					end := k + inst.Len
					if end > len(oc) {
						end = len(oc)
					}
					if inst.PCRel > 0 && k+inst.PCRelOff+inst.PCRel <= len(oc) {
						a, b := k+inst.PCRelOff, k+inst.PCRelOff+inst.PCRel
						same = string(oc[k:a]) == string(nc[k:a]) && string(oc[b:end]) == string(nc[b:end])
						if same && inst.PCRel == 4 {
							od := int64(int32(binary.LittleEndian.Uint32(oc[a:])))
							nd := int64(int32(binary.LittleEndian.Uint32(nc[a:])))
							to := uint64(int64(f.Entry) + int64(end) + od)
							tn := uint64(int64(g.Entry) + int64(end) + nd)
							if s := secOf(old, to); s != nil && want[s.Name] {
								if ns := new.Sects[s.Name]; ns != nil {
									local = append(local, sample{to - s.Addr, int64(tn-ns.Addr) - int64(to-s.Addr)})
									localSect = append(localSect, s.Name)
								}
							}
						}
					} else {
						same = string(oc[k:end]) == string(nc[k:end])
					}
					k = end
				}
				if !same {
					continue
				}
				for q, smp := range local {
					samples[localSect[q]] = append(samples[localSect[q]], smp)
				}
			}
		}(w)
	}
	wg.Wait()
	samples := map[string][]sample{}
	for _, pw := range perWorker {
		for n, ss := range pw {
			samples[n] = append(samples[n], ss...)
		}
	}
	out := map[string]*ShiftTable{}
	for name, ss := range samples {
		sort.Slice(ss, func(a, b int) bool {
			if ss[a].off != ss[b].off {
				return ss[a].off < ss[b].off
			}
			return ss[a].delta < ss[b].delta
		})
		t := &ShiftTable{Samples: len(ss)}
		cur := int64(0)
		for _, s := range ss {
			if s.delta != cur {
				t.Offs = append(t.Offs, s.off)
				t.Deltas = append(t.Deltas, s.delta)
				cur = s.delta
			}
		}
		out[name] = t
	}
	return out
}
