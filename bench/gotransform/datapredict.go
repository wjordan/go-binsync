package main

import (
	"encoding/binary"
	"flag"
	"fmt"
)

// cmdDataPredict analyses the residual of the data sections (.rodata,
// .noptrdata, .data): how much is content that moved (size-sorted symbol
// insertion) vs absolute pointers whose targets moved, and how much a
// predicted section (old blocks re-laid out through the content map, with
// absolute pointers re-targeted through the function map / data maps) leaves
// for the correction.
func cmdDataPredict(args []string) {
	fs := flag.NewFlagSet("datapredict", flag.ExitOnError)
	which := fs.String("enc", "bhz", "encoders")
	block := fs.Int("block", 16, "block size")
	fs.Parse(args)
	if fs.NArg() < 2 {
		fatal("usage: datapredict OLD NEW")
	}
	old, err := loadBin(fs.Arg(0))
	must(err)
	new, err := loadBin(fs.Arg(1))
	must(err)
	tag := "dp-" + baseName(old.Path) + "-" + baseName(new.Path)
	fmt.Printf("== datapredict %s -> %s\n", old.Path, new.Path)
	m := matchFuncs(old, new)
	olo, ohi := imageRange(old)
	names := []string{".rodata", ".noptrdata", ".data"}
	dmaps := map[string]*DataMap{}
	for _, n := range names {
		dmaps[n], _, _ = buildDataMapMasked(old, new, n, *block)
	}
	mp := &mapper{src: old, dst: new, srcToDst: m.OldToNew, dataMaps: dmaps, shiftTabs: deriveShiftTables(old, new, m, []string{".bss", ".noptrbss"})}
	for _, n := range names {
		os, ns := old.Sects[n], new.Sects[n]
		dm := dmaps[n]
		// pointer census in old
		var byClass [rcNum]int
		total := 0
		for o := 0; o+8 <= len(os.Data); o += 8 {
			v := binary.LittleEndian.Uint64(os.Data[o:])
			if v < olo || v >= ohi {
				continue
			}
			total++
			_, cls := mp.mapAddr(v, nil)
			byClass[cls]++
		}
		fmt.Printf("%s: %d bytes, %d aligned qwords point into the image:", n, len(os.Data), total)
		for c := 0; c < int(rcNum); c++ {
			if byClass[c] > 0 {
				fmt.Printf(" %s=%d", refClassNames[c], byClass[c])
			}
		}
		fmt.Println()

		// predicted section: re-lay old blocks through the map, then rewrite pointers
		pred := make([]byte, len(ns.Data))
		placed := 0
		for i := 0; i*dm.Block < len(os.Data); i++ {
			o := i * dm.Block
			e := o + dm.Block
			if e > len(os.Data) {
				e = len(os.Data)
			}
			p := int64(o) + dm.Delta[i]
			if p < 0 || int(p)+(e-o) > len(pred) {
				continue
			}
			copy(pred[p:], os.Data[o:e])
			placed++
		}
		predNoPtr := append([]byte(nil), pred...)
		// pointer rewrite: walk old qwords, write mapped value at mapped position
		var same, fixed, wrong, unmapped int
		for o := 0; o+8 <= len(os.Data); o += 8 {
			v := binary.LittleEndian.Uint64(os.Data[o:])
			if v < olo || v >= ohi {
				continue
			}
			p := int64(o) + dm.Delta[o/dm.Block]
			if p < 0 || int(p)+8 > len(pred) {
				continue
			}
			nv, cls := mp.mapAddr(v, nil)
			if cls == rcTextUnmatch || cls == rcOutside {
				unmapped++
				continue
			}
			binary.LittleEndian.PutUint64(pred[p:], nv)
			actual := binary.LittleEndian.Uint64(ns.Data[p:])
			switch {
			case actual == v:
				same++
				if nv != v {
					wrong++
				}
			case actual == nv:
				fixed++
			default:
				wrong++
			}
		}
		fmt.Printf("  pointer rewrite: unchanged-in-new=%d re-targeted-correctly=%d mispredicted=%d unmapped=%d (map: %s)\n", same, fixed, wrong, unmapped, dm)
		base := measure(tag+n+"-base", os.Data, ns.Data, *which)
		d0 := measure(tag+n+"-relayout", predNoPtr, ns.Data, *which)
		d1 := measure(tag+n+"-relayout+ptr", pred, ns.Data, *which)
		fmt.Printf("  baseline old->new:            %s\n", base)
		fmt.Printf("  blocks re-laid (no ptr fix):  %s\n", d0)
		fmt.Printf("  blocks re-laid + ptr rewrite: %s\n", d1)
	}
}
