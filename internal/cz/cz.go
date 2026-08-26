// Package cz is the compression used inside patches and blobs: a one-byte
// codec tag, and an encoder that picks the smallest of the candidates it is
// willing to spend time on.
//
// Pure-Go zstd is 6-14 % worse than the zstd CLI at -19 on patch streams;
// pure-Go brotli at quality 11 is 6-8 % better, and slow enough that it is
// only worth it below BrotliMax. See docs/DESIGN.md 3.5.
package cz

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Codec tags. They are written into patch frame tables and must not be
// renumbered.
const (
	Raw    = 0
	Zstd   = 1
	Brotli = 2
)

// BrotliMax is the largest stream brotli-11 is tried on. Quality 11 runs at
// roughly 0.4 MB/s, so above this the encode time stops being free.
const BrotliMax = 4 << 20

var (
	encOnce sync.Once
	zEnc    *zstd.Encoder
	zDec    *zstd.Decoder
	decOnce sync.Once
)

func zstdEncoder() *zstd.Encoder {
	encOnce.Do(func() {
		zEnc, _ = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedBestCompression),
			zstd.WithWindowSize(8<<20),
			zstd.WithEncoderConcurrency(1))
	})
	return zEnc
}

func zstdDecoder() *zstd.Decoder {
	decOnce.Do(func() {
		zDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<30))
	})
	return zDec
}

// CompressZstd compresses with zstd only. Blobs use this: they are tens of
// MB and must stream at a sane rate.
func CompressZstd(src []byte) []byte { return zstdEncoder().EncodeAll(src, nil) }

// Compress returns the smallest of the codecs worth trying for src, and the
// tag that names it. It never returns something larger than src itself.
func Compress(src []byte) (codec byte, out []byte) {
	codec, out = Raw, src
	if z := CompressZstd(src); len(z) < len(out) {
		codec, out = Zstd, z
	}
	if len(src) <= BrotliMax {
		var buf bytes.Buffer
		buf.Grow(len(out))
		w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
		if _, err := w.Write(src); err == nil && w.Close() == nil && buf.Len() < len(out) {
			codec, out = Brotli, buf.Bytes()
		}
	}
	return codec, out
}

// readAll reads r into buf, refusing to grow past n+1 bytes so that a
// hostile stream cannot make the decoder allocate without bound.
func readAll(r io.Reader, buf []byte, n int) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			if len(buf) > n {
				return buf, fmt.Errorf("stream longer than the declared %d bytes", n)
			}
			buf = append(buf, 0)[:len(buf)]
		}
		m, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+m]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return buf, err
		}
	}
}

// Decompress reverses Compress. n is the expected decompressed length; it
// bounds the allocation, and a stream that does not produce exactly n bytes
// is an error.
func Decompress(codec byte, src []byte, n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("cz: negative output length %d", n)
	}
	var out []byte
	var err error
	switch codec {
	case Raw:
		out = src
	case Zstd:
		out, err = zstdDecoder().DecodeAll(src, make([]byte, 0, n))
	case Brotli:
		out = make([]byte, 0, n)
		out, err = readAll(brotli.NewReader(bytes.NewReader(src)), out, n)
	default:
		return nil, fmt.Errorf("cz: unknown codec %d", codec)
	}
	if err != nil {
		return nil, fmt.Errorf("cz: codec %d: %w", codec, err)
	}
	if len(out) != n {
		return nil, fmt.Errorf("cz: codec %d: decompressed %d bytes, want %d", codec, len(out), n)
	}
	return out, nil
}
