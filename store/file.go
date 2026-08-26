package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func init() { Register("file", openFile) }

// lockName is the compare-and-swap lock, in the store root. It is not a key:
// keys never start with a dot.
const lockName = ".binsync.lock"

// fileStore is a directory. Publishing is write-temp, fsync, rename, fsync
// dir, so a target polling the directory never observes a partial object.
type fileStore struct {
	root string
	raw  string
}

// openFile accepts file:///abs/dir and, because a publisher writing to a
// working copy will type it, file://./rel/dir (url.Parse puts the first
// segment of a relative path in Host).
func openFile(u *url.URL) (Store, error) {
	root := u.Path
	if u.Host != "" && u.Host != "localhost" {
		root = u.Host + u.Path
	}
	if root == "" {
		return nil, fmt.Errorf("store: %q: file:// needs a directory", u.String())
	}
	return &fileStore{root: filepath.Clean(root), raw: u.String()}, nil
}

func (s *fileStore) URL() string  { return s.raw }
func (s *fileStore) Close() error { return nil }

// checkKey rejects anything that is not a clean relative slash path, so a key
// out of a pointer can neither reach outside the store nor name the lockfile.
func checkKey(key string) error {
	if key == "" || key != path.Clean(key) || strings.HasPrefix(key, "/") || strings.HasPrefix(key, ".") {
		return fmt.Errorf("store: key %q: want a clean relative path", key)
	}
	return nil
}

func (s *fileStore) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

// fileETag identifies a version of a file. The inode is in it because rename
// gives the new file a new one even on a filesystem whose timestamps are too
// coarse to separate two publishes of the same-sized object.
func fileETag(fi fs.FileInfo) string {
	tag := fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		tag += fmt.Sprintf("-%d", st.Ino)
	}
	return tag
}

func (s *fileStore) Get(ctx context.Context, key string, o GetOptions) (*Object, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("store: %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("store: get %s: %w", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("store: get %s: %w", key, err)
	}
	if fi.IsDir() {
		f.Close()
		return nil, fmt.Errorf("store: %s: %w", key, ErrNotFound)
	}
	etag := fileETag(fi)
	if o.IfNoneMatch != "" && o.IfNoneMatch == etag {
		f.Close()
		return nil, fmt.Errorf("store: %s: %w", key, ErrNotModified)
	}
	if o.Off > 0 || o.Len > 0 {
		off := min(o.Off, fi.Size())
		n := fi.Size() - off
		if o.Len > 0 {
			n = min(n, o.Len)
		}
		return &Object{Body: sectionCloser{io.NewSectionReader(f, off, n), f}, Size: n, ETag: etag}, nil
	}
	return &Object{Body: f, Size: fi.Size(), ETag: etag}, nil
}

type sectionCloser struct {
	io.Reader
	io.Closer
}

// Put ignores ContentType, CacheControl and Size: a directory carries no
// metadata and needs no length up front.
func (s *fileStore) Put(ctx context.Context, key string, r io.Reader, o PutOptions) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: put %s: %w", key, err)
	}
	if o.IfMatch != nil {
		unlock, err := s.lock(ctx)
		if err != nil {
			return fmt.Errorf("store: put %s: %w", key, err)
		}
		defer unlock()
		cur := ""
		switch fi, err := os.Stat(p); {
		case err == nil:
			cur = fileETag(fi)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("store: put %s: %w", key, err)
		}
		if cur != *o.IfMatch {
			return fmt.Errorf("store: put %s: etag is %q, not %q: %w", key, cur, *o.IfMatch, ErrPreconditionFailed)
		}
	}
	tmp, err := writeTemp(dir, filepath.Base(p), r)
	if err != nil {
		return fmt.Errorf("store: put %s: %w", key, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: put %s: %w", key, err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: put %s: %w", key, err)
	}
	return nil
}

// writeTemp writes r to <base>.tmp.<rand> in dir and returns its name. The
// data is on disk before the caller renames it into place.
func writeTemp(dir, base string, r io.Reader) (string, error) {
	f, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	err = func() error {
		if _, err := io.Copy(f, r); err != nil {
			return err
		}
		if err := f.Chmod(0o644); err != nil { // CreateTemp makes it 0600
			return err
		}
		return f.Sync()
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// fsyncDir makes the rename itself durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// lock serialises the stat-then-rename of a compare-and-swap Put across
// processes. Only CAS writers take it: every other object is immutable and
// written under a key nobody else writes.
func (s *fileStore) lock(ctx context.Context) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.root, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for delay := time.Millisecond; ; delay = min(2*delay, 20*time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}
