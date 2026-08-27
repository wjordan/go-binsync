package delta

import (
	"encoding/hex"
	"fmt"

	"github.com/zeebo/blake3"

	"github.com/wjordan/go-binsync/internal/cz"
)

// Magic is the first four bytes of every patch.
const Magic = "BSZ1"

// Transforms. The number is written into the patch; a decoder that meets one
// it does not implement returns ErrUnsupportedTransform, and the target
// falls back to the blob.
const (
	// TransformPlain is the general-purpose codec: a bsdiff-class delta over
	// the whole file. It works on anything.
	TransformPlain = 0
	// TransformGoAMD64 is the Go-aware predict-then-correct codec for
	// stripped, non-PIE linux/amd64 binaries built by the supported Go
	// release (gobin.SupportedGo).
	TransformGoAMD64 = 1

	maxTransform = TransformGoAMD64
)

// FrameSize is the uncompressed size of one patch frame. Frames are
// independently decodable so that a partial fetch resumes at a frame
// boundary and is verified before it is used.
const FrameSize = 8 << 20

// maxPatchSize bounds a patch the decoder will parse, so that a corrupt
// header cannot become an allocation.
const maxPatchSize = 1 << 34

// Frame describes one compressed piece of the patch body.
type Frame struct {
	Off   int64 // offset of this frame in the uncompressed body
	Len   int64 // uncompressed length
	ZLen  int64 // compressed length, as stored
	Codec byte  // cz codec tag
	B3    Hash  // BLAKE3 of the compressed bytes, as stored
}

// Header is the plaintext prologue of a patch: enough to decide whether the
// patch applies here, how large the result will be, and how to verify each
// frame of it as it arrives.
type Header struct {
	Transform byte
	Flags     byte
	From, To  Hash
	OldSize   int64
	NewSize   int64
	Frames    []Frame

	// BodyOff is the offset in the patch at which frame 0 starts.
	BodyOff int64
}

// ErrUnsupportedTransform means the patch was produced by a newer codec than
// this build implements. The caller should fetch the blob instead.
type ErrUnsupportedTransform struct{ Transform byte }

func (e *ErrUnsupportedTransform) Error() string {
	return fmt.Sprintf("delta: patch uses transform %d, this build implements up to %d", e.Transform, maxTransform)
}

func marshalHeader(h *Header, body []byte) []byte {
	w := &wbuf{}
	w.raw([]byte(Magic))
	w.raw([]byte{h.Transform, h.Flags})
	f := &wbuf{}
	f.raw(h.From[:])
	f.raw(h.To[:])
	f.u(uint64(h.OldSize))
	f.u(uint64(h.NewSize))
	f.u(uint64(len(h.Frames)))
	for _, fr := range h.Frames {
		f.u(uint64(fr.Off))
		f.u(uint64(fr.Len))
		f.u(uint64(fr.ZLen))
		f.raw([]byte{fr.Codec})
		f.raw(fr.B3[:])
	}
	w.bytes(f.b)
	return append(w.b, body...)
}

// ParseHeader reads the prologue of a patch. b may be a prefix of the patch:
// it needs only the magic, the fixed bytes and the header record.
func ParseHeader(b []byte) (*Header, error) {
	if len(b) < 6 || string(b[:4]) != Magic {
		return nil, fmt.Errorf("%w: bad magic", errCorrupt)
	}
	h := &Header{Transform: b[4], Flags: b[5]}
	r := &rbuf{b: b[6:]}
	rec := r.bytes()
	if r.err != nil {
		return nil, r.err
	}
	h.BodyOff = int64(len(b) - len(r.b))
	f := &rbuf{b: rec}
	copy(h.From[:], f.take(32))
	copy(h.To[:], f.take(32))
	h.OldSize = int64(f.un(maxPatchSize, "old size"))
	h.NewSize = int64(f.un(maxPatchSize, "new size"))
	n := f.un(1<<20, "frame count")
	var off int64
	for i := uint64(0); i < n && f.err == nil; i++ {
		var fr Frame
		fr.Off = int64(f.un(maxPatchSize, "frame offset"))
		fr.Len = int64(f.un(FrameSize, "frame length"))
		fr.ZLen = int64(f.un(maxPatchSize, "frame compressed length"))
		fr.Codec = f.byte()
		copy(fr.B3[:], f.take(32))
		if fr.Off != off {
			f.fail("frame %d starts at %d, want %d", i, fr.Off, off)
			break
		}
		off += fr.Len
		h.Frames = append(h.Frames, fr)
	}
	if err := f.done(); err != nil {
		return nil, err
	}
	return h, nil
}

// BodySize is the uncompressed length of the patch body.
func (h *Header) BodySize() int64 {
	var n int64
	for _, f := range h.Frames {
		n += f.Len
	}
	return n
}

// Size is the length of the whole patch object.
func (h *Header) Size() int64 {
	n := h.BodyOff
	for _, f := range h.Frames {
		n += f.ZLen
	}
	return n
}

// frameBody compresses body into frames and returns the frame table and the
// concatenated compressed bytes.
func frameBody(body []byte) ([]Frame, []byte) {
	var frames []Frame
	var out []byte
	for off := 0; off < len(body) || off == 0 && len(body) == 0; off += FrameSize {
		end := min(off+FrameSize, len(body))
		chunk := body[off:end]
		codec, z := cz.Compress(chunk)
		frames = append(frames, Frame{
			Off: int64(off), Len: int64(len(chunk)), ZLen: int64(len(z)),
			Codec: codec, B3: hashOf(z),
		})
		out = append(out, z...)
		if end == len(body) {
			break
		}
	}
	return frames, out
}

// readBody verifies and decompresses every frame of a whole patch.
func readBody(h *Header, patch []byte) ([]byte, error) {
	if int64(len(patch)) != h.Size() {
		return nil, fmt.Errorf("%w: patch is %d bytes, header describes %d", errCorrupt, len(patch), h.Size())
	}
	body := make([]byte, 0, h.BodySize())
	off := h.BodyOff
	for i, f := range h.Frames {
		z := patch[off : off+f.ZLen]
		if hashOf(z) != f.B3 {
			return nil, fmt.Errorf("%w: frame %d fails its hash", errCorrupt, i)
		}
		out, err := cz.Decompress(f.Codec, z, int(f.Len))
		if err != nil {
			return nil, fmt.Errorf("%w: frame %d: %v", errCorrupt, i, err)
		}
		body = append(body, out...)
		off += f.ZLen
	}
	return body, nil
}

// Hash is a BLAKE3-256 digest, the identity of a release. It is the same
// 32 bytes as release.Hash; the codec keeps its own name so that it depends
// on nothing above it.
type Hash [32]byte

// String renders the hash the way the pointer does.
func (h Hash) String() string {
	if h == (Hash{}) {
		return ""
	}
	return "b3:" + hex.EncodeToString(h[:])
}

func hashOf(b []byte) Hash { return Hash(blake3.Sum256(b)) }
