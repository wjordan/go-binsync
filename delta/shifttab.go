package delta

import (
	"encoding/binary"
	"sort"
	"sync"

	"github.com/wjordan/go-binsync/delta/gobin"
	"golang.org/x/arch/x86/x86asm"
)

// A shiftTable is the piecewise-constant old-to-new offset map of a section
// that has no content to match on: .bss and .noptrbss. The encoder derives
// it from the references of functions whose code did not change -- it can
// see both binaries and read what the new displacement points at -- and
// transmits it. It is tiny because bss symbols are size-sorted, so an
// inserted variable shifts everything after it by one constant.
type shiftTable struct {
	Offs    []uint64
	Deltas  []int64
	Samples int
}

func (t *shiftTable) Map(off uint64) uint64 {
	if t == nil || len(t.Offs) == 0 {
		return off
	}
	i := sort.Search(len(t.Offs), func(i int) bool { return t.Offs[i] > off }) - 1
	if i < 0 {
		return off
	}
	return uint64(int64(off) + t.Deltas[i])
}

func (t *shiftTable) encode() []byte {
	w := &wbuf{}
	var po uint64
	var pd int64
	for i := range t.Offs {
		w.u(t.Offs[i] - po)
		w.s(t.Deltas[i] - pd)
		po, pd = t.Offs[i], t.Deltas[i]
	}
	return w.b
}

func decodeShiftTable(b []byte) (*shiftTable, error) {
	r := &rbuf{b: b}
	t := &shiftTable{}
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

var bssSects = []string{".bss", ".noptrbss"}

// deriveShiftTables walks the matched functions whose code is unchanged and
// compares where their PC-relative references point in each release.
func deriveShiftTables(old, new *gobin.Bin, m *match) map[string]*shiftTable {
	type sample struct {
		off   uint64
		delta int64
	}
	want := map[string]bool{}
	for _, n := range bssSects {
		want[n] = true
	}
	secOf := func(b *gobin.Bin, t uint64) *gobin.Section {
		if s := b.SectionOf(t); s != nil {
			return s
		}
		if t > 0 {
			return b.SectionOf(t - 1) // one-past-the-end references
		}
		return nil
	}
	perWorker := make([]map[string][]sample, predictWorkers)
	var wg sync.WaitGroup
	for w := range predictWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			samples := map[string][]sample{}
			perWorker[w] = samples
			for j := w; j < len(new.Funcs); j += predictWorkers {
				g := new.Funcs[j]
				i := m.NewToOld[j]
				if i < 0 {
					continue
				}
				f := old.Funcs[i]
				if f.Size() != g.Size() {
					continue
				}
				oc, nc := old.FuncBytes(f), new.FuncBytes(g)
				local := map[string][]sample{}
				same := true
				for k := 0; k < len(oc) && same; {
					inst, err := x86asm.Decode(oc[k:], 64)
					if err != nil || inst.Len == 0 {
						same = oc[k] == nc[k]
						k++
						continue
					}
					end := min(k+inst.Len, len(oc))
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
									local[s.Name] = append(local[s.Name], sample{to - s.Addr, int64(tn-ns.Addr) - int64(to-s.Addr)})
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
				for n, ss := range local {
					samples[n] = append(samples[n], ss...)
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
	out := map[string]*shiftTable{}
	for name, ss := range samples {
		sort.Slice(ss, func(a, b int) bool {
			if ss[a].off != ss[b].off {
				return ss[a].off < ss[b].off
			}
			return ss[a].delta < ss[b].delta
		})
		t := &shiftTable{Samples: len(ss)}
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
