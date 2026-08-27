package release

import (
	"fmt"
	"runtime"
	"sync"

	"binsync/internal/cz"
)

// A blob is the whole binary, compressed in independent frames of FrameSize
// input bytes each. Independence is what makes a blob fetchable with eight
// parallel range requests and resumable at a frame boundary, which is worth
// far more on a lossy link than the compression ratio a single stream would
// gain (docs/DESIGN.md 5). Each frame takes whichever codec is smallest for
// it, so a blob is not a file any single decompressor reads.

// EncodeBlob compresses data into blob frames and returns the object bytes
// and the frame table to publish with them. Frames are compressed in
// parallel -- each is independent, so the result does not depend on how many
// workers ran, and compressing a 94 MB binary is 10 s rather than 70 s.
func EncodeBlob(h Hash, data []byte) ([]byte, *Blob) {
	return encodeBlob(h, data, FrameSize)
}

// encodeBlob is EncodeBlob with the frame size as a parameter, so that the
// tests can cover the frame-boundary cases without compressing tens of
// megabytes to do it.
func encodeBlob(h Hash, data []byte, frame int) ([]byte, *Blob) {
	b := &Blob{Key: BlobKey(h), Size: int64(len(data))}
	n := max((len(data)+frame-1)/frame, 1)
	b.Frames = make([]Frame, n)
	zs := make([][]byte, n)

	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i := range n {
		off := i * frame
		end := min(off+frame, len(data))
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			codec, z := cz.Compress(data[off:end])
			zs[i] = z
			b.Frames[i] = Frame{Off: int64(off), Len: int64(end - off), ZLen: int64(len(z)),
				Codec: codec, B3: HashBytes(z)}
		}()
	}
	wg.Wait()

	var out []byte
	for i := range n {
		b.Frames[i].ZOff = int64(len(out))
		out = append(out, zs[i]...)
	}
	b.Size = int64(len(out))
	return out, b
}

// DecodeFrame verifies one fetched frame and returns its plain bytes.
func DecodeFrame(f Frame, z []byte) ([]byte, error) {
	if int64(len(z)) != f.ZLen {
		return nil, fmt.Errorf("release: blob frame at %d is %d bytes, the pointer says %d", f.Off, len(z), f.ZLen)
	}
	if HashBytes(z) != f.B3 {
		return nil, fmt.Errorf("release: blob frame at %d fails its hash", f.Off)
	}
	return cz.Decompress(f.Codec, z, int(f.Len))
}

// PlainSize is the uncompressed length the frame table describes, and the
// length of the binary a blob reconstructs.
func (b *Blob) PlainSize() int64 {
	var n int64
	for _, f := range b.Frames {
		n += f.Len
	}
	return n
}

// Check rejects a frame table that does not describe one contiguous file, so
// that a corrupt pointer cannot make a fetcher allocate or write out of
// bounds.
func (b *Blob) Check() error {
	var off, zoff int64
	for i, f := range b.Frames {
		if f.Off != off || f.Len < 0 || f.Len > FrameSize || f.ZOff != zoff || f.ZLen < 0 {
			return fmt.Errorf("release: blob frame %d is out of order", i)
		}
		off += f.Len
		zoff += f.ZLen
	}
	if zoff != b.Size {
		return fmt.Errorf("release: blob frames total %d bytes, the pointer says %d", zoff, b.Size)
	}
	return nil
}
