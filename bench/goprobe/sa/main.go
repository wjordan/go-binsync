// sa: time and memory of index/suffixarray.New over a binary (bounds a pure-Go bsdiff encoder).
package main

import (
	"fmt"
	"index/suffixarray"
	"os"
	"runtime"
	"time"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	t0 := time.Now()
	sa := suffixarray.New(data)
	dt := time.Since(t0)
	runtime.ReadMemStats(&after)
	// a few lookups so the index isn't dead
	n := len(sa.Lookup([]byte("net/http"), -1))
	fmt.Printf("suffixarray.New: input=%d B build=%.2fs heap_delta=%.1f MB sys=%.1f MB (lookups net/http=%d)\n",
		len(data), dt.Seconds(), float64(after.HeapAlloc-before.HeapAlloc)/1e6, float64(after.Sys)/1e6, n)
}
