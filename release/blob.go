package release

import (
	"fmt"

	"binsync/internal/cz"
)

// A blob is the whole binary, compressed in independent frames of FrameSize
// input bytes each. Independence is what makes a blob fetchable with eight
// parallel range requests and resumable at a frame boundary, which is worth
// far more on a lossy link than the compression ratio a single stream would
// gain (docs/DESIGN.md 5).

// EncodeBlob compresses data into blob frames and returns the object bytes
// and the frame table to publish with them.
func EncodeBlob(h Hash, data []byte) ([]byte, *Blob) {
	b := &Blob{Key: BlobKey(h), Size: int64(len(data))}
	var out []byte
	for off := 0; off < len(data) || off == 0; off += FrameSize {
		end := min(off+FrameSize, len(data))
		z := cz.CompressZstd(data[off:end])
		b.Frames = append(b.Frames, Frame{
			Off: int64(off), Len: int64(end - off),
			ZOff: int64(len(out)), ZLen: int64(len(z)),
			B3: HashBytes(z),
		})
		out = append(out, z...)
		if end == len(data) {
			break
		}
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
	return cz.Decompress(cz.Zstd, z, int(f.Len))
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
