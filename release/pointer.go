package release

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// PointerKey is the one mutable object in a store.
const PointerKey = "latest.json"

// Format is the store-layout version written into every pointer. A target
// that reads a larger value keeps its current binary and logs.
const Format = 1

// MaxChain is how many patch edges a pointer carries, and therefore how far
// behind a target can be and still follow the chain.
const MaxChain = 8

// FrameSize is the uncompressed size of one blob frame.
const FrameSize = 8 << 20

// Frame is one independently fetchable, independently verified piece of a
// blob: [Off, Off+Len) of the plain binary, stored as ZLen compressed bytes
// at the same relative position in the object.
type Frame struct {
	Off  int64 `json:"off"`
	Len  int64 `json:"len"`
	ZOff int64 `json:"zoff"`
	ZLen int64 `json:"zlen"`
	B3   Hash  `json:"b3"`
}

// Blob locates the full compressed binary of a release.
type Blob struct {
	Key    string  `json:"key"`
	Size   int64   `json:"size"`
	Frames []Frame `json:"frames"`
}

// Release is the head of a stream: one exact binary.
type Release struct {
	Hash    Hash   `json:"hash"`
	Version string `json:"version,omitempty"`
	Size    int64  `json:"size"`
	Blob    *Blob  `json:"blob,omitempty"`
}

// Edge is one published patch, turning From's bytes into To's.
type Edge struct {
	From Hash   `json:"from"`
	To   Hash   `json:"to"`
	Key  string `json:"key"`
	Size int64  `json:"size"`
	B3   Hash   `json:"b3"`
}

// Pointer is the content of <store>/latest.json.
type Pointer struct {
	Format int     `json:"format"`
	Seq    int64   `json:"seq"`
	Head   Release `json:"head"`
	Chain  []Edge  `json:"chain"`

	// ETag is what the store returned when this pointer was fetched; it is
	// not part of the serialised object.
	ETag string `json:"-"`
}

// PatchKey is the object key of the patch from -> to.
func PatchKey(from, to Hash) string {
	return "patches/" + from.Short() + "-" + to.Short() + ".bsz"
}

// BlobKey is the object key of the blob of h.
func BlobKey(h Hash) string { return "blobs/" + hex.EncodeToString(h[:]) + ".zst" }

// NewSeq returns a pointer sequence number for now: wall clock in ms.
func NewSeq() int64 { return time.Now().UnixMilli() }

// Marshal renders the pointer as the JSON stored under PointerKey.
func (p *Pointer) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParsePointer parses a stored pointer and checks its invariants.
func ParsePointer(b []byte) (*Pointer, error) {
	var p Pointer
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("release: pointer: %w", err)
	}
	if p.Format > Format {
		return nil, &UnknownFormatError{Got: p.Format, Known: Format}
	}
	if p.Head.Hash.IsZero() {
		return nil, fmt.Errorf("release: pointer has no head hash")
	}
	if p.Head.Size < 0 {
		return nil, fmt.Errorf("release: pointer head size %d", p.Head.Size)
	}
	for i, e := range p.Chain {
		if e.From.IsZero() || e.To.IsZero() || e.Key == "" {
			return nil, fmt.Errorf("release: pointer chain[%d] incomplete", i)
		}
		if i == 0 && e.To != p.Head.Hash {
			return nil, fmt.Errorf("release: pointer chain[0].to != head")
		}
		if i > 0 && e.To != p.Chain[i-1].From {
			return nil, fmt.Errorf("release: pointer chain[%d] does not link to chain[%d]", i, i-1)
		}
	}
	return &p, nil
}

// UnknownFormatError is returned for a pointer written by a newer publisher.
type UnknownFormatError struct{ Got, Known int }

func (e *UnknownFormatError) Error() string {
	return fmt.Sprintf("release: store format %d is newer than %d", e.Got, e.Known)
}
