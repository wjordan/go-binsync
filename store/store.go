// Package store is the object interface go-binsync publishes to and polls: a
// flat key space under one URL, with conditional reads, ranged reads and a
// compare-and-swap write for the single mutable object.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
)

// ErrNotFound is returned by Get and Stat for a key the store does not hold.
var ErrNotFound = errors.New("store: not found")

// ErrNotModified is returned by Get when GetOptions.IfNoneMatch matched.
var ErrNotModified = errors.New("store: not modified")

// ErrPreconditionFailed is returned by Put when PutOptions.IfMatch did not
// match: another publisher replaced the object first.
var ErrPreconditionFailed = errors.New("store: precondition failed")

// ErrReadOnly is returned by Put on a store that cannot publish (https://).
var ErrReadOnly = errors.New("store: read-only")

// GetOptions modifies a read.
type GetOptions struct {
	// IfNoneMatch, when set, makes Get return ErrNotModified if the object's
	// current ETag equals it.
	IfNoneMatch string
	// Off and Len, when Len > 0, request only [Off, Off+Len) of the object.
	Off, Len int64
}

// PutOptions modifies a write.
type PutOptions struct {
	// IfMatch, when non-nil, makes Put a compare-and-swap: the write only
	// happens if the object's current ETag equals *IfMatch. The empty string
	// means "the object must not exist".
	IfMatch *string
	// ContentType and CacheControl are set on stores that carry them.
	ContentType  string
	CacheControl string
	// Size is the number of bytes r will yield, or -1 if unknown. Stores
	// that need a length ahead of time buffer when it is -1.
	Size int64
}

// Object is what Get returns.
type Object struct {
	Body io.ReadCloser
	// Size is the length of Body, or -1 if unknown.
	Size int64
	// ETag identifies this version of the object, for IfNoneMatch/IfMatch.
	ETag string
}

// Store is a flat key space.
type Store interface {
	// Get reads a key. A ranged Get returns only the requested bytes and its
	// Size is the length of that range.
	Get(ctx context.Context, key string, o GetOptions) (*Object, error)
	// Put writes a key. It is atomic: a reader sees either the whole
	// previous object or the whole new one.
	Put(ctx context.Context, key string, r io.Reader, o PutOptions) error
	// URL is the store's address, for logs.
	URL() string
	// Close releases any connection the store holds.
	Close() error
}

// Opener builds a store from a parsed URL.
type Opener func(u *url.URL) (Store, error)

var (
	mu      sync.RWMutex
	openers = map[string]Opener{}
)

// Register adds a URL scheme. Backends whose dependencies are heavy live in
// their own package and register themselves from init, so a program that
// does not import them does not link them.
func Register(scheme string, o Opener) {
	mu.Lock()
	defer mu.Unlock()
	openers[scheme] = o
}

// Open resolves a store URL. Recognised schemes are file://, https://,
// ssh:// and — when go-binsync/store/s3 is imported — s3://.
func Open(raw string) (Store, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("store: %q: %w", raw, err)
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("store: %q: needs a scheme (file://, s3://, https://, ssh://)", raw)
	}
	mu.RLock()
	open := openers[u.Scheme]
	known := make([]string, 0, len(openers))
	for s := range openers {
		known = append(known, s+"://")
	}
	mu.RUnlock()
	if open == nil {
		return nil, fmt.Errorf("store: %q: unsupported scheme %q (have %s)", raw, u.Scheme, strings.Join(known, " "))
	}
	return open(u)
}

// GetAll reads a whole object into memory.
func GetAll(ctx context.Context, s Store, key string) ([]byte, string, error) {
	obj, err := s.Get(ctx, key, GetOptions{})
	if err != nil {
		return nil, "", err
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	return b, obj.ETag, err
}
