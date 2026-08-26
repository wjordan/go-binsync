// bcj: classic LZMA-SDK / xz x86 BCJ filter (E8/E9 rel32 -> absolute), applied
// only inside the ELF .text section. Usage: bcj enc|dec IN OUT
// (translation of x86_Convert from LZMA SDK Bra86.c; ip = 0 at .text start)
package main

import (
	"debug/elf"
	"fmt"
	"os"
)

var kMaskToAllowedStatus = [8]bool{true, true, true, false, true, false, false, false}
var kMaskToBitNumber = [8]uint32{0, 1, 2, 2, 3, 3, 3, 3}

func test86MSByte(b byte) bool { return b == 0 || b == 0xFF }

func x86Convert(data []byte, ip uint32, encoding bool) {
	size := len(data)
	if size < 5 {
		return
	}
	ip += 5
	bufferPos := 0
	prevPosT := -1
	prevMask := uint32(0)
	for {
		p := bufferPos
		limit := size - 4
		for p < limit && (data[p]&0xFE) != 0xE8 {
			p++
		}
		bufferPos = p
		if p >= limit {
			break
		}
		d := bufferPos - prevPosT
		if d > 3 {
			prevMask = 0
		} else {
			prevMask = (prevMask << uint(d-1)) & 7
			if prevMask != 0 {
				b := data[p+4-int(kMaskToBitNumber[prevMask])]
				if !kMaskToAllowedStatus[prevMask] || test86MSByte(b) {
					prevPosT = bufferPos
					prevMask = ((prevMask << 1) & 7) | 1
					bufferPos++
					continue
				}
			}
		}
		prevPosT = bufferPos
		if test86MSByte(data[p+4]) {
			src := uint32(data[p+4])<<24 | uint32(data[p+3])<<16 | uint32(data[p+2])<<8 | uint32(data[p+1])
			var dest uint32
			for {
				if encoding {
					dest = ip + uint32(bufferPos) + src
				} else {
					dest = src - (ip + uint32(bufferPos))
				}
				if prevMask == 0 {
					break
				}
				index := kMaskToBitNumber[prevMask] * 8
				b := byte(dest >> (24 - index))
				if !test86MSByte(b) {
					break
				}
				src = dest ^ ((1 << (32 - index)) - 1)
			}
			data[p+4] = ^byte(((dest >> 24) & 1) - 1)
			data[p+3] = byte(dest >> 16)
			data[p+2] = byte(dest >> 8)
			data[p+1] = byte(dest)
			bufferPos += 5
		} else {
			prevMask = ((prevMask << 1) & 7) | 1
			bufferPos++
		}
	}
}

func main() {
	mode, in, out := os.Args[1], os.Args[2], os.Args[3]
	data, err := os.ReadFile(in)
	if err != nil {
		panic(err)
	}
	f, err := elf.NewFile(bytesReader(data))
	if err != nil {
		panic(err)
	}
	text := f.Section(".text")
	if text == nil {
		panic("no .text")
	}
	seg := data[text.Offset : text.Offset+text.Size]
	x86Convert(seg, 0, mode == "enc")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("bcj %s: .text [%d,+%d) processed\n", mode, text.Offset, text.Size)
}
