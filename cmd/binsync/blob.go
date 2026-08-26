package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"binsync/release"
	"binsync/store"
)

// blobParallel is how many ranged requests a blob is fetched with. One TCP
// stream carries ~1.2 Mbit/s under 1 % loss whatever the link rate, so eight
// streams are most of a blob download (docs/DESIGN.md 5).
const blobParallel = 8

// fetchBlob downloads a whole release with parallel ranged requests over its
// frame table, verifying each frame as it lands and the release at the end.
// This is the publisher's copy of the fetch: it needs the previous release to
// encode against, and a cold cache is the one time it downloads one.
func fetchBlob(ctx context.Context, s store.Store, rel release.Release) ([]byte, error) {
	b := rel.Blob
	if err := b.Check(); err != nil {
		return nil, err
	}
	out := make([]byte, b.PlainSize())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan release.Frame)
	errs := make(chan error, blobParallel)
	var wg sync.WaitGroup
	for range min(blobParallel, len(b.Frames)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				plain, err := fetchFrame(ctx, s, b.Key, f)
				if err != nil {
					errs <- err
					cancel()
					return
				}
				copy(out[f.Off:], plain)
			}
		}()
	}
	for _, f := range b.Frames {
		select {
		case work <- f:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	if got := release.HashBytes(out); got != rel.Hash {
		return nil, fmt.Errorf("%s hashes to %s, want %s", b.Key, got, rel.Hash)
	}
	return out, nil
}

func fetchFrame(ctx context.Context, s store.Store, key string, f release.Frame) ([]byte, error) {
	obj, err := s.Get(ctx, key, store.GetOptions{Off: f.ZOff, Len: f.ZLen})
	if err != nil {
		return nil, fmt.Errorf("fetching %s at %d: %w", key, f.ZOff, err)
	}
	z, err := io.ReadAll(io.LimitReader(obj.Body, f.ZLen+1))
	obj.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("fetching %s at %d: %w", key, f.ZOff, err)
	}
	return release.DecodeFrame(f, z)
}

// exists reports whether the store holds key, asking for one byte of it.
func exists(ctx context.Context, s store.Store, key string) (bool, error) {
	obj, err := s.Get(ctx, key, store.GetOptions{Len: 1})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking for %s: %w", key, err)
	}
	obj.Body.Close()
	return true, nil
}

// sameFrames reports whether two frame tables describe the same object byte
// for byte. Re-uploading a blob a previous run left unfinished is only safe
// while the compressor still produces the bytes the pointer's hashes were
// computed over.
func sameFrames(a, b []release.Frame) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
