package main

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/arch/x86/x86asm"
)

func cmdInventory(args []string) {
	if len(args) < 1 {
		fatal("usage: inventory OLD [NEW]")
	}
	old, err := loadBin(args[0])
	must(err)
	printInventory(old)
	if len(args) < 2 {
		return
	}
	new, err := loadBin(args[1])
	must(err)
	printInventory(new)
	comparePair(old, new)
}

func printInventory(b *Bin) {
	fmt.Printf("== %s (%d bytes)\n", b.Path, len(b.File))
	fmt.Printf("sections (allocated, by address):\n")
	for _, s := range b.Order {
		fmt.Printf("  %-18s addr=%#-10x off=%#-9x size=%9d\n", s.Name, s.Addr, s.Off, s.Size)
	}
	p := b.Pcln
	fmt.Printf("layout=%s pclntab: magic ok, nfunc=%d nfiles=%d minLC=%d ptrSize=%d section=%d bytes (go:func.*/findfunctab inside .gopclntab: %v)\n", b.Layout, p.NFunc, p.NFiles, p.MinLC, p.PtrSize, len(p.Data), p.Inside)
	for _, r := range []struct {
		n string
		r Range
	}{{"funcnametab", p.Funcnametab}, {"cutab", p.Cutab}, {"filetab", p.Filetab}, {"pctab", p.Pctab}, {"functab+_func", p.Functab}, {"go:func.*", p.Gofunc}, {"findfunctab", p.Findfunctab}} {
		fmt.Printf("  %-14s off=%9d len=%9d\n", r.n, r.r.Off, r.r.Len)
	}
	fmt.Printf("moduledata: text=%#x etext=%#x minpc=%#x maxpc=%#x types=%#x etypes=%#x rodata=%#x gofunc=%#x findfunctab=%#x epclntab=%#x typelinks=%d itablinks=%d\n",
		b.Mod.Text, b.Mod.Etext, b.Mod.Minpc, b.Mod.Maxpc, b.Mod.Types, b.Mod.Etypes, b.Mod.Rodata, b.Mod.Gofunc, b.Mod.Findfunctab, b.Mod.Epclntab, b.Mod.Typelinks.Len, b.Mod.Itablinks.Len)
	first, last := b.Funcs[0], b.Funcs[len(b.Funcs)-1]
	fmt.Printf("functions: %d; first %s @%#x; last %s @%#x end=%#x; .text end=%#x (gap after last func: %d bytes)\n",
		len(b.Funcs), first.Name, first.Entry, last.Name, last.Entry, last.End, b.Text.Addr+b.Text.Size, int64(b.Text.Addr+b.Text.Size)-int64(last.End))
	aligned, unaligned := 0, 0
	var unalignedNames []string
	dups := 0
	sizes := map[string]int{}
	cats := map[string]int{}
	var total uint64
	for _, f := range b.Funcs {
		if f.Entry%32 == 0 {
			aligned++
		} else {
			unaligned++
			if len(unalignedNames) < 10 {
				unalignedNames = append(unalignedNames, fmt.Sprintf("%s@%#x", f.Name, f.Entry))
			}
		}
		total += f.Size()
		for _, c := range nameCategories(f.Name) {
			cats[c]++
		}
	}
	for n, fs := range b.ByName {
		if len(fs) > 1 {
			dups++
			if len(sizes) < 10 {
				sizes[n] = len(fs)
			}
		}
	}
	fmt.Printf("entry alignment: %d on 32-byte boundary, %d not (%v)\n", aligned, unaligned, unalignedNames)
	// x86asm decode failures per function
	type ff struct {
		n string
		c int
	}
	var fails []ff
	totalFails := 0
	for _, f := range b.Funcs {
		code := b.funcBytes(f)
		c := 0
		for i := 0; i < len(code); {
			inst, err := x86asm.Decode(code[i:], 64)
			if err != nil || inst.Len == 0 {
				c++
				i++
				continue
			}
			i += inst.Len
		}
		if c > 0 {
			fails = append(fails, ff{f.Name, c})
			totalFails += c
		}
	}
	sort.Slice(fails, func(i, j int) bool { return fails[i].c > fails[j].c })
	fmt.Printf("x86asm decode failures: %d bytes in %d functions; top:", totalFails, len(fails))
	for i := 0; i < len(fails) && i < 12; i++ {
		fmt.Printf(" %s=%d", fails[i].n, fails[i].c)
	}
	fmt.Println()
	fmt.Printf("sum of function sizes (entry to next entry): %d = %.2f%% of .text\n", total, 100*float64(total)/float64(b.Text.Size))
	fmt.Printf("duplicate names: %d distinct names with >1 function (examples: %v)\n", dups, sizes)
	var keys []string
	for k := range cats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("name categories:")
	for _, k := range keys {
		fmt.Printf(" %s=%d", k, cats[k])
	}
	fmt.Println()
}

func comparePair(old, new *Bin) {
	fmt.Printf("== pair %s -> %s\n", old.Path, new.Path)
	oldNames := map[string]int{}
	newNames := map[string]int{}
	for _, f := range old.Funcs {
		oldNames[f.Name]++
	}
	for _, f := range new.Funcs {
		newNames[f.Name]++
	}
	var onlyOld, onlyNew []string
	for n := range oldNames {
		if newNames[n] == 0 {
			onlyOld = append(onlyOld, n)
		}
	}
	for n := range newNames {
		if oldNames[n] == 0 {
			onlyNew = append(onlyNew, n)
		}
	}
	sort.Strings(onlyOld)
	sort.Strings(onlyNew)
	fmt.Printf("names only in old (%d): %s\n", len(onlyOld), strings.Join(onlyOld, ", "))
	fmt.Printf("names only in new (%d): %s\n", len(onlyNew), strings.Join(onlyNew, ", "))
	m := matchFuncs(old, new)
	fmt.Printf("match: exact-name=%d normalised-name=%d size+content=%d unmatched-new=%d unmatched-old=%d (new has %d, old has %d)\n",
		m.Exact, m.Norm, m.Content, m.Unmatched, m.UnmatchedOld, len(new.Funcs), len(old.Funcs))
	var sizeChanged, moved, contentChanged, unchanged int
	var sizeChangedNames []string
	var oldSz, newSz uint64
	for j, f := range new.Funcs {
		i := m.NewToOld[j]
		if i < 0 {
			continue
		}
		o := old.Funcs[i]
		if o.Size() != f.Size() {
			sizeChanged++
			oldSz += o.Size()
			newSz += f.Size()
			sizeChangedNames = append(sizeChangedNames, fmt.Sprintf("%s(%d->%d)", f.Name, o.Size(), f.Size()))
		}
		if o.Entry != f.Entry {
			moved++
		}
		if string(old.funcBytes(o)) == string(new.funcBytes(f)) {
			unchanged++
		} else if contentHash(old.funcBytes(o)) != contentHash(new.funcBytes(f)) {
			contentChanged++
		}
	}
	fmt.Printf("matched functions: size changed=%d (old %d B -> new %d B) moved=%d bytes-identical=%d content-changed(after masking PC-rel fields)=%d\n",
		sizeChanged, oldSz, newSz, moved, unchanged, contentChanged)
	fmt.Printf("size-changed: %s\n", strings.Join(sizeChangedNames, ", "))
	// matched by normalised name / content: show a few examples
	shown := 0
	for j, f := range new.Funcs {
		if (m.How[j] == 'n' || m.How[j] == 'c') && shown < 12 {
			fmt.Printf("  matched by %c: new %s <- old %s\n", m.How[j], f.Name, old.Funcs[m.NewToOld[j]].Name)
			shown++
		}
	}
	for j, f := range new.Funcs {
		if m.NewToOld[j] < 0 {
			fmt.Printf("  unmatched new: %s (%d B)\n", f.Name, f.Size())
		}
	}
	for i, f := range old.Funcs {
		if m.OldToNew[i] < 0 {
			fmt.Printf("  unmatched old: %s (%d B)\n", f.Name, f.Size())
		}
	}
	lay := encodeLayout(old, new, m)
	fmt.Printf("layout table: %d functions, raw delta encoding %d B, zstd -19 %d B\n", len(new.Funcs), len(lay), zstdSize("layout-"+baseName(old.Path)+"-"+baseName(new.Path), lay))
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
