// Package release holds the on-store metadata of a release stream (the
// pointer and the patch chain), the plan a target derives from it, and the
// atomic install/revert of a binary at a fixed path.
package release

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zeebo/blake3"
)

// Hash is a BLAKE3-256 digest. Its text form is "b3:<64 hex>"; the zero
// value is "no hash" and marshals to the empty string.
type Hash [32]byte

// HashBytes returns the BLAKE3-256 of b.
func HashBytes(b []byte) Hash {
	return Hash(blake3.Sum256(b))
}

// HashReader returns the BLAKE3-256 of everything r yields.
func HashReader(r io.Reader) (Hash, error) {
	h := blake3.New()
	if _, err := io.Copy(h, r); err != nil {
		return Hash{}, err
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// HashFile returns the BLAKE3-256 of the file at path.
func HashFile(path string) (Hash, error) {
	f, err := os.Open(path)
	if err != nil {
		return Hash{}, err
	}
	defer f.Close()
	return HashReader(f)
}

// IsZero reports whether h is the zero hash.
func (h Hash) IsZero() bool { return h == Hash{} }

// String returns "b3:<hex>", or "" for the zero hash.
func (h Hash) String() string {
	if h.IsZero() {
		return ""
	}
	return "b3:" + hex.EncodeToString(h[:])
}

// Short returns the first 8 hex digits, the form used in object keys.
func (h Hash) Short() string {
	if h.IsZero() {
		return ""
	}
	return hex.EncodeToString(h[:4])
}

// ParseHash parses "b3:<64 hex>".
func ParseHash(s string) (Hash, error) {
	var h Hash
	if s == "" {
		return h, nil
	}
	rest, ok := strings.CutPrefix(s, "b3:")
	if !ok {
		return h, fmt.Errorf("release: hash %q: want a b3: prefix", s)
	}
	b, err := hex.DecodeString(rest)
	if err != nil || len(b) != 32 {
		return h, fmt.Errorf("release: hash %q: want 64 hex digits", s)
	}
	copy(h[:], b)
	return h, nil
}

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	v, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = v
	return nil
}
