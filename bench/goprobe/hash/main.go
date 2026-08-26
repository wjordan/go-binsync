// hash: single-threaded BLAKE3 and SHA-256 throughput over a file (min of 5 runs).
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"lukechampine.com/blake3"
)

func best(name string, data []byte, f func([]byte)) {
	var bestD time.Duration
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		f(data)
		d := time.Since(t0)
		if i == 0 || d < bestD {
			bestD = d
		}
	}
	fmt.Printf("%-10s %d B in %7.2f ms = %7.1f MB/s (single goroutine)\n", name, len(data), float64(bestD.Microseconds())/1e3, float64(len(data))/bestD.Seconds()/1e6)
}

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	best("sha256", data, func(b []byte) { sha256.Sum256(b) })
	best("blake3", data, func(b []byte) { blake3.Sum256(b) })
	// blake3 with the 64 KiB-chunk streaming hasher (what a chunk-level index would do)
	best("blake3-64K", data, func(b []byte) {
		for i := 0; i < len(b); i += 65536 {
			j := i + 65536
			if j > len(b) {
				j = len(b)
			}
			blake3.Sum256(b[i:j])
		}
	})
}
