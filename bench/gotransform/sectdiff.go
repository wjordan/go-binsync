package main

import (
	"bytes"
	"fmt"
	"sync"
)

func cmdSectdiff(args []string) {
	if len(args) < 2 {
		fatal("usage: sectdiff OLD NEW")
	}
	old, err := loadBin(args[0])
	must(err)
	new, err := loadBin(args[1])
	must(err)
	tag := baseName(old.Path) + "-" + baseName(new.Path)
	fmt.Printf("== sectdiff %s -> %s\n", old.Path, new.Path)
	type row struct {
		name string
		d    Delta
	}
	rows := make([]row, len(old.Order))
	var wg sync.WaitGroup
	for i, s := range old.Order {
		if s.Data == nil {
			continue
		}
		ns := new.Sects[s.Name]
		if ns == nil {
			continue
		}
		wg.Add(1)
		go func(i int, s, ns *Section) {
			defer wg.Done()
			var d Delta
			if bytes.Equal(s.Data, ns.Data) {
				d = Delta{OldLen: int64(len(s.Data)), NewLen: int64(len(ns.Data))}
			} else {
				d = measure("sd-"+tag+"-"+s.Name, s.Data, ns.Data, "bhz")
			}
			rows[i] = row{s.Name, d}
		}(i, s, ns)
	}
	wg.Add(1)
	var whole Delta
	go func() {
		defer wg.Done()
		whole = measure("sd-"+tag+"-whole", old.File, new.File, "bhz")
	}()
	wg.Wait()
	fmt.Printf("%-18s %10s %10s %10s %10s %10s %10s\n", "section", "size", "rawdiff", "runs", "bsdiff", "hdiffz", "zstd")
	var sum Delta
	for _, r := range rows {
		if r.name == "" {
			continue
		}
		d := r.d
		fmt.Printf("%-18s %10d %10d %10d %10d %10d %10d\n", r.name, d.NewLen, d.Diff, d.Runs, d.Bsdiff, d.Hdiffz, d.Zstd)
		if d.Bsdiff > 0 {
			sum.Bsdiff += d.Bsdiff
			sum.Hdiffz += d.Hdiffz
			sum.Zstd += d.Zstd
			sum.Diff += d.Diff
			sum.Runs += d.Runs
		}
	}
	fmt.Printf("%-18s %10s %10d %10d %10d %10d %10d\n", "SUM(sections)", "", sum.Diff, sum.Runs, sum.Bsdiff, sum.Hdiffz, sum.Zstd)
	fmt.Printf("%-18s %10d %10d %10d %10d %10d %10d  (bsdiff %.1fs hdiffz %.1fs zstd %.1fs)\n", "WHOLE FILE", whole.NewLen, whole.Diff, whole.Runs, whole.Bsdiff, whole.Hdiffz, whole.Zstd, whole.TBsdiff.Seconds(), whole.THdiffz.Seconds(), whole.TZstd.Seconds())
}
