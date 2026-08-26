package main

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// typeResidual classifies the stage-2 residual of the type section (encoder
// debugging aid): for every differing run it finds the old block mapped to
// that position, the enclosing descriptor / itab (by the walker's roots) and
// prints a histogram by (region, block status) plus the first n runs.
func typeResidual(old, new *Bin, dmaps map[string]*DataMap, pred []byte, n int) {
	tsec := old.sectionOf(old.Mod.Types)
	if tsec == nil {
		return
	}
	os, ns := old.Sects[tsec.Name], new.Sects[tsec.Name]
	dm := dmaps[tsec.Name]
	if os == nil || ns == nil || dm == nil {
		return
	}
	// inverse map: new offset -> old block, via the sorted new starts of the old blocks
	type span struct {
		newOff int64
		i      int
	}
	spans := make([]span, 0, len(dm.Delta))
	for i, d := range dm.Delta {
		spans = append(spans, span{int64(i*dm.Block) + d, i})
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].newOff < spans[b].newOff })
	oldBlockAt := func(p int64) int {
		k := sort.Search(len(spans), func(k int) bool { return spans[k].newOff > p }) - 1
		if k < 0 || p >= spans[k].newOff+int64(dm.Block) {
			return -1
		}
		return spans[k].i
	}
	// old descriptor starts (typelinked range) and itab range, by old offset
	var descs []uint64
	om := old.Mod
	if om.Typedesclen > 0 {
		a := om.Types + 8
		end := om.Types + om.Typedesclen
		d := os.Data
		for a < end {
			a = (a + 7) &^ 7
			o := a - os.Addr
			if o+sizeType > uint64(len(d)) {
				break
			}
			descs = append(descs, o)
			sz := descSize(d, o, old.Layout)
			if sz <= 0 {
				break
			}
			a += uint64(sz)
		}
	}
	itabLo, itabHi := om.Types+om.Itaboffset-os.Addr, om.Types+om.Itaboffset+om.Itabsize-os.Addr
	region := func(o uint64) string {
		if o >= itabLo && o < itabHi {
			return "itab"
		}
		k := sort.Search(len(descs), func(k int) bool { return descs[k] > o }) - 1
		if k >= 0 {
			sz := descSize(os.Data, descs[k], old.Layout)
			if sz > 0 && o < descs[k]+uint64(sz) {
				f := o - descs[k]
				switch {
				case f < 48:
					return fmt.Sprintf("desc.Type+%d", f)
				default:
					return "desc.tail"
				}
			}
			if o < om.Types+om.Typedesclen-os.Addr {
				return "desc.pad"
			}
		}
		return "other(names/non-typelinked)"
	}
	np := pred[ns.Off : ns.Off+ns.Size]
	nd := ns.Data
	hist := map[string]int{}
	histBytes := map[string]int{}
	shown := 0
	for p := 0; p < len(nd); {
		if p >= len(np) || np[p] == nd[p] {
			p++
			continue
		}
		e := p
		for e < len(nd) && (e >= len(np) || np[e] != nd[e]) {
			e++
		}
		i := oldBlockAt(int64(p))
		key := "no-old-block"
		if i >= 0 {
			o := uint64(i*dm.Block) + uint64(int64(p)-spans[0].newOff*0) - uint64(dm.Delta[i])
			_ = o
			oo := uint64(int64(p) - dm.Delta[i])
			status := "matched"
			if !dm.Matched[i] {
				status = "unmatched"
				if dm.Ambiguous != nil && dm.Ambiguous[i] {
					status = "ambiguous"
				}
			}
			key = region(oo) + "/" + status
		}
		hist[key]++
		histBytes[key] += e - p
		if shown < n {
			var oldBytes []byte
			if i >= 0 {
				oo := int64(p) - dm.Delta[i]
				if oo >= 0 && int(oo)+(e-p) <= len(os.Data) {
					oldBytes = os.Data[oo : int(oo)+(e-p)]
				}
			}
			fmt.Printf("  resid %s new+%d len %d: pred %x new %x old %x\n", key, p, e-p, np[p:e], nd[p:e], oldBytes)
			shown++
		}
		p = e
	}
	keys := make([]string, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return histBytes[keys[a]] > histBytes[keys[b]] })
	fmt.Printf("  type-section residual by region/block status (runs, bytes):")
	for _, k := range keys {
		fmt.Printf(" %s=%d(%dB)", k, hist[k], histBytes[k])
	}
	fmt.Println()
	_ = binary.LittleEndian
}
