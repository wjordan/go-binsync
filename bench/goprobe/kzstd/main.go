// kzstd: can klauspost/compress zstd express `zstd --patch-from`? Compress NEW
// with OLD as a raw-content dictionary and round-trip it.
package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"
)

func main() {
	old, _ := os.ReadFile(os.Args[1])
	neu, _ := os.ReadFile(os.Args[2])
	for _, ws := range []int{1 << 26, 1 << 27, 1 << 28} {
		for _, lvl := range []zstd.EncoderLevel{zstd.SpeedBestCompression, zstd.SpeedBetterCompression, zstd.SpeedDefault} {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(lvl),
				zstd.WithWindowSize(ws),
				zstd.WithEncoderConcurrency(1),
				zstd.WithEncoderDictRaw(1, old), // raw content dictionary (no zstd dict header)
			)
			if err != nil {
				fmt.Printf("window=%d level=%v: encoder error: %v\n", ws, lvl, err)
				continue
			}
			t0 := time.Now()
			out := enc.EncodeAll(neu, nil)
			dt := time.Since(t0)
			dec, err := zstd.NewReader(nil, zstd.WithDecoderDictRaw(1, old), zstd.WithDecoderMaxWindow(1<<30), zstd.WithDecoderMaxMemory(1<<31))
			if err != nil {
				fmt.Printf("decoder error: %v\n", err)
				continue
			}
			t1 := time.Now()
			back, err := dec.DecodeAll(out, nil)
			dd := time.Since(t1)
			ok := err == nil && bytes.Equal(back, neu)
			fmt.Printf("window=%dMiB level=%-18v patch=%9d B encode=%6.2fs decode=%5.2fs roundtrip=%v %v\n",
				ws>>20, lvl, len(out), dt.Seconds(), dd.Seconds(), ok, errStr(err))
			enc.Close()
			dec.Close()
		}
	}
	// no-dictionary baseline for reference
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
	t0 := time.Now()
	out := enc.EncodeAll(neu, nil)
	fmt.Printf("no-dict SpeedBestCompression: %d B in %.2fs\n", len(out), time.Since(t0).Seconds())
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
