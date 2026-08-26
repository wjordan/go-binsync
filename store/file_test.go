package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func openFileStore(t *testing.T) Store {
	t.Helper()
	s, err := Open("file://" + t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFileSuite(t *testing.T) {
	t.Parallel()
	StoreSuite(t, openFileStore(t))
}

func TestFileOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rel, err := filepath.Rel(mustGetwd(t), dir)
	if err != nil {
		t.Skipf("no relative path to %s: %v", dir, err)
	}
	for _, raw := range []string{"file://" + dir, "file://localhost" + dir, "file://./" + filepath.ToSlash(rel)} {
		s, err := Open(raw)
		if err != nil {
			t.Fatalf("open %s: %v", raw, err)
		}
		if err := s.Put(context.Background(), "k", strings.NewReader("v"), PutOptions{}); err != nil {
			t.Fatalf("put via %s: %v", raw, err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "k")); err != nil || string(b) != "v" {
			t.Fatalf("put via %s landed as %q, %v", raw, b, err)
		}
		if s.URL() != raw {
			t.Fatalf("URL() = %q, want %q", s.URL(), raw)
		}
		s.Close()
		os.Remove(filepath.Join(dir, "k"))
	}
	if _, err := Open("file://"); err == nil {
		t.Fatal("file:// with no directory was accepted")
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestFileRejectsBadKeys(t *testing.T) {
	t.Parallel()
	s := openFileStore(t)
	ctx := context.Background()
	for _, key := range []string{"", "/abs", "../escape", "a/../../escape", "./a", "a//b", lockName} {
		if _, err := s.Get(ctx, key, GetOptions{}); err == nil {
			t.Errorf("get %q was accepted", key)
		}
		if err := s.Put(ctx, key, strings.NewReader("x"), PutOptions{}); err == nil {
			t.Errorf("put %q was accepted", key)
		}
	}
}

func TestFileGet(t *testing.T) {
	t.Parallel()
	s := openFileStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("0123456789"), 100) // 1000 B
	if err := s.Put(ctx, "blobs/x.zst", bytes.NewReader(body), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}

	for _, tc := range []struct {
		name string
		o    GetOptions
		want []byte
	}{
		{"whole", GetOptions{}, body},
		{"window", GetOptions{Off: 10, Len: 5}, body[10:15]},
		{"to the end", GetOptions{Off: 990}, body[990:]},
		{"past the end", GetOptions{Off: 995, Len: 100}, body[995:]},
		{"beyond the end", GetOptions{Off: 4000, Len: 10}, nil},
		{"first byte", GetOptions{Len: 1}, body[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := s.Get(ctx, "blobs/x.zst", tc.o)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer obj.Body.Close()
			got, err := io.ReadAll(obj.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %d bytes, want %d", len(got), len(tc.want))
			}
			if int(obj.Size) != len(tc.want) {
				t.Fatalf("Size = %d, read %d bytes", obj.Size, len(got))
			}
		})
	}

	if _, err := s.Get(ctx, "blobs", GetOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of a directory: %v, want ErrNotFound", err)
	}
}

func TestFileETagTracksTheFile(t *testing.T) {
	t.Parallel()
	s := openFileStore(t)
	ctx := context.Background()
	put := func(v string) string {
		t.Helper()
		if err := s.Put(ctx, "latest.json", strings.NewReader(v), PutOptions{}); err != nil {
			t.Fatalf("put: %v", err)
		}
		_, etag, err := GetAll(ctx, s, "latest.json")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return etag
	}
	// Same length, and possibly the same coarse mtime: the etag must still
	// change, or a poller would sit on a stale pointer.
	first, second := put("aaa"), put("bbb")
	if first == second {
		t.Fatalf("etag %q survived a rewrite", first)
	}
	if _, err := s.Get(ctx, "latest.json", GetOptions{IfNoneMatch: first}); err != nil {
		t.Fatalf("get with the old etag: %v", err)
	}
	if _, err := s.Get(ctx, "latest.json", GetOptions{IfNoneMatch: second}); !errors.Is(err, ErrNotModified) {
		t.Fatalf("get with the current etag: %v, want ErrNotModified", err)
	}
}

// TestFileNoPartialReads checks the write-temp-then-rename: a reader polling
// a key that is being rewritten sees one whole version or the other, never a
// prefix of the new one.
func TestFileNoPartialReads(t *testing.T) {
	t.Parallel()
	s := openFileStore(t)
	ctx := context.Background()
	const size = 64 << 10
	versions := [][]byte{bytes.Repeat([]byte{'a'}, size), bytes.Repeat([]byte{'b'}, size)}
	if err := s.Put(ctx, "latest.json", bytes.NewReader(versions[0]), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := range 30 {
			v := versions[i%2]
			if err := s.Put(ctx, "latest.json", &trickle{b: v}, PutOptions{}); err != nil {
				t.Errorf("put %d: %v", i, err)
				return
			}
		}
	}()

	reads := 0
	for {
		select {
		case <-done:
			wg.Wait()
			if reads == 0 {
				t.Fatal("the writer finished before a single read")
			}
			return
		default:
		}
		b, _, err := GetAll(ctx, s, "latest.json")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(b) != size || bytes.Count(b, b[:1]) != size {
			t.Fatalf("read %d bytes of a mixed object", len(b))
		}
		reads++
	}
}

// trickle yields b in small chunks so a write that was not staged in a temp
// file would be caught mid-flight.
type trickle struct {
	b   []byte
	off int
}

func (r *trickle) Read(p []byte) (int, error) {
	if r.off == len(r.b) {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 4096)], r.b[r.off:])
	r.off += n
	runtime.Gosched()
	return n, nil
}

func TestFileLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open("file://" + dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	absent, stale := "", "stale"
	if err := s.Put(ctx, "latest.json", strings.NewReader("one"), PutOptions{IfMatch: &absent}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Put(ctx, "latest.json", strings.NewReader("two"), PutOptions{IfMatch: &stale}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale replace: %v, want ErrPreconditionFailed", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
	t.Logf("store directory: %v", names)
}

// TestFileCASLock pins what the compare-and-swap lock does and does not
// cover: a CAS Put waits for it, an immutable Put never touches it.
func TestFileCASLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open("file://" + dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	held, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	if err := s.Put(ctx, "blobs/x.zst", strings.NewReader("immutable"), PutOptions{}); err != nil {
		t.Fatalf("put of an immutable key blocked on the lock: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	absent := ""
	if err := s.Put(cancelled, "latest.json", strings.NewReader("x"), PutOptions{IfMatch: &absent}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cas put with a cancelled context: %v, want context.Canceled", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Put(ctx, "latest.json", strings.NewReader("one"), PutOptions{IfMatch: &absent})
	}()
	select {
	case err := <-done:
		t.Fatalf("cas put ran while the lock was held: %v", err)
	case <-time.After(5 * time.Millisecond):
	}
	syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	held.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cas put after the lock was released: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cas put did not proceed after the lock was released")
	}
	if b, _, err := GetAll(ctx, s, "latest.json"); err != nil || string(b) != "one" {
		t.Fatalf("latest.json = %q, %v", b, err)
	}
}
