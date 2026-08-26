// gobsdiff: pure-Go bsdiff (gabstv/go-bsdiff) encode time/size and patch round-trip.
package main

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/gabstv/go-bsdiff/pkg/bsdiff"
	"github.com/gabstv/go-bsdiff/pkg/bspatch"
)

func main() {
	old, _ := os.ReadFile(os.Args[1])
	neu, _ := os.ReadFile(os.Args[2])
	t0 := time.Now()
	patch, err := bsdiff.Bytes(old, neu)
	dt := time.Since(t0)
	if err != nil {
		fmt.Printf("bsdiff error: %v\n", err)
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t1 := time.Now()
	back, err := bspatch.Bytes(old, patch)
	dd := time.Since(t1)
	fmt.Printf("go-bsdiff: patch=%d B encode=%.2fs decode=%.2fs roundtrip=%v sys=%.0f MB heap_peak~=%.0f MB\n",
		len(patch), dt.Seconds(), dd.Seconds(), err == nil && bytes.Equal(back, neu), float64(ms.Sys)/1e6, float64(ms.TotalAlloc)/1e6)
}
