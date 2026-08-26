package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"
)

// StoreSuite is the behaviour every writable backend has to share. A backend
// in another package (s3, ssh) calls it against a live store; keys are put
// under a random prefix so a shared bucket can host several runs at once.
func StoreSuite(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("storesuite/%016x/", rand.Uint64())
	body := bytes.Repeat([]byte("binsync"), 300) // 2100 B

	put := func(t *testing.T, key string, b []byte, o PutOptions) error {
		t.Helper()
		return s.Put(ctx, prefix+key, bytes.NewReader(b), o)
	}
	get := func(t *testing.T, key string, o GetOptions) ([]byte, string) {
		t.Helper()
		obj, err := s.Get(ctx, prefix+key, o)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		defer obj.Body.Close()
		b, err := io.ReadAll(obj.Body)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if obj.Size >= 0 && int(obj.Size) != len(b) {
			t.Fatalf("get %s: Size %d, read %d bytes", key, obj.Size, len(b))
		}
		return b, obj.ETag
	}

	t.Run("RoundTrip", func(t *testing.T) {
		if err := put(t, "blobs/a.zst", body, PutOptions{Size: int64(len(body))}); err != nil {
			t.Fatalf("put: %v", err)
		}
		got, etag := get(t, "blobs/a.zst", GetOptions{})
		if !bytes.Equal(got, body) {
			t.Fatalf("read back %d bytes, want %d", len(got), len(body))
		}
		if etag == "" {
			t.Fatal("no etag")
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		if _, err := s.Get(ctx, prefix+"blobs/missing.zst", GetOptions{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get missing: %v, want ErrNotFound", err)
		}
	})

	t.Run("Range", func(t *testing.T) {
		got, _ := get(t, "blobs/a.zst", GetOptions{Off: 100, Len: 50})
		if !bytes.Equal(got, body[100:150]) {
			t.Fatalf("range [100,150) = %q", got)
		}
		got, _ = get(t, "blobs/a.zst", GetOptions{Off: int64(len(body)) - 10, Len: 1000})
		if !bytes.Equal(got, body[len(body)-10:]) {
			t.Fatalf("range past the end = %q", got)
		}
	})

	t.Run("Conditional", func(t *testing.T) {
		_, etag := get(t, "blobs/a.zst", GetOptions{})
		if _, err := s.Get(ctx, prefix+"blobs/a.zst", GetOptions{IfNoneMatch: etag}); !errors.Is(err, ErrNotModified) {
			t.Fatalf("if-none-match %q: %v, want ErrNotModified", etag, err)
		}
		if got, _ := get(t, "blobs/a.zst", GetOptions{IfNoneMatch: etag + "x"}); !bytes.Equal(got, body) {
			t.Fatal("if-none-match with a stale etag did not return the object")
		}
	})

	t.Run("CASConcurrent", func(t *testing.T) {
		casConcurrent(t, s, prefix+"concurrent.json")
	})

	t.Run("CAS", func(t *testing.T) {
		absent, wrong := "", "not-the-etag"
		if err := put(t, "latest.json", []byte("one"), PutOptions{IfMatch: &absent}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := put(t, "latest.json", []byte("two"), PutOptions{IfMatch: &absent}); !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("create over an existing key: %v, want ErrPreconditionFailed", err)
		}
		got, etag := get(t, "latest.json", GetOptions{})
		if string(got) != "one" {
			t.Fatalf("after the failed create: %q", got)
		}
		if err := put(t, "latest.json", []byte("two"), PutOptions{IfMatch: &etag}); err != nil {
			t.Fatalf("replace: %v", err)
		}
		if err := put(t, "latest.json", []byte("three"), PutOptions{IfMatch: &wrong}); !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("replace with a stale etag: %v, want ErrPreconditionFailed", err)
		}
		if got, _ := get(t, "latest.json", GetOptions{}); string(got) != "two" {
			t.Fatalf("after the failed replace: %q", got)
		}
	})
}

// CASConcurrent is the guarantee the pointer rests on: of N writers that all
// saw the same version, exactly one replaces it.
func casConcurrent(t *testing.T, s Store, key string) {
	ctx := context.Background()
	absent := ""
	if err := s.Put(ctx, key, bytes.NewReader([]byte("seed")), PutOptions{IfMatch: &absent}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	obj, err := s.Get(ctx, key, GetOptions{})
	if err != nil {
		t.Fatalf("get seed: %v", err)
	}
	obj.Body.Close()

	const writers = 8
	errs := make([]error, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			etag := obj.ETag
			<-start // every writer races from the same version
			errs[i] = s.Put(ctx, key, bytes.NewReader(fmt.Appendf(nil, "writer %d", i)), PutOptions{IfMatch: &etag})
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrPreconditionFailed):
			t.Fatalf("writer %d: %v, want nil or ErrPreconditionFailed", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent compare-and-swaps won, want 1", won, writers)
	}
}
