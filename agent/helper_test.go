package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/release"
	"github.com/wjordan/go-binsync/store"
)

// relSize is large enough that a patch between two releases is far smaller
// than the blob, which is what makes MakePlan choose the chain.
const relSize = 128 << 10

// relBytes is one release: the same incompressible body every time, with a
// marker so that consecutive releases differ in a few bytes.
func relBytes(n int) []byte {
	b := make([]byte, relSize)
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}
	copy(b[1024:], fmt.Sprintf("release %d\x00", n))
	return b
}

// fixture is a file:// release stream plus the target binary that polls it.
type fixture struct {
	t    *testing.T
	dir  string
	path string
	log  *sink

	ptr      *release.Pointer
	prev     []byte
	prevHash release.Hash
	seq      int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, dir: t.TempDir(), path: filepath.Join(t.TempDir(), "server"), log: &sink{}}
}

func (f *fixture) url() string { return "file://" + f.dir }

func (f *fixture) config() Config {
	return Config{Store: f.url(), Path: f.path, Logger: slog.New(f.log)}
}

// agent builds an agent with the timings squeezed to milliseconds; the
// defaults are minutes, which no test can wait for.
func (f *fixture) agent(h Hooks) *agent {
	f.t.Helper()
	a, err := newAgent(f.config(), h)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { a.st.Close() })
	a.checkFor, a.checkEvery = 20*time.Millisecond, time.Millisecond
	a.blobWait, a.blobRetry, a.retry = 200*time.Millisecond, time.Millisecond, time.Millisecond
	return a
}

// install puts data at the target path, as a previous update would have.
func (f *fixture) install(data []byte) {
	f.t.Helper()
	if err := os.WriteFile(f.path, data, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) binary() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// publish is `go-binsync publish`: the blob, then the patch from the previous
// head, then the pointer naming both (docs/DESIGN.md 4.4).
func (f *fixture) publish(data []byte) release.Hash {
	f.t.Helper()
	h := release.HashBytes(data)
	f.seq++
	head := release.Release{
		Hash:    h,
		Version: fmt.Sprintf("v0.0.%d", f.seq),
		Size:    int64(len(data)),
		Blob:    f.putBlob(data, h),
	}
	var chain []release.Edge
	if f.prev != nil {
		patch, err := delta.Encode(f.prev, data, delta.Options{})
		if err != nil {
			f.t.Fatal(err)
		}
		key := release.PatchKey(f.prevHash, h)
		f.put(key, patch)
		edge := release.Edge{From: f.prevHash, To: h, Key: key, Size: int64(len(patch)), B3: release.HashBytes(patch)}
		chain = append([]release.Edge{edge}, f.ptr.Chain...)
		if len(chain) > release.MaxChain {
			chain = chain[:release.MaxChain]
		}
	}
	f.ptr = &release.Pointer{Format: release.Format, Seq: f.seq, Head: head, Chain: chain}
	f.putPointer()
	f.prev, f.prevHash = data, h
	return h
}

func (f *fixture) putBlob(data []byte, h release.Hash) *release.Blob {
	f.t.Helper()
	obj, b := release.EncodeBlob(h, data)
	f.put(b.Key, obj)
	return b
}

func (f *fixture) putPointer() {
	f.t.Helper()
	b, err := f.ptr.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	f.put(release.PointerKey, b)
}

func (f *fixture) put(key string, b []byte) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.key(key)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.key(key), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) get(key string) []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.key(key))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) remove(key string) {
	f.t.Helper()
	if err := os.Remove(f.key(key)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) key(key string) string { return filepath.Join(f.dir, filepath.FromSlash(key)) }

func (f *fixture) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hooks records the order the loop called things in.
type hooks struct {
	mu    sync.Mutex
	calls []string
	check error
}

func (h *hooks) restart(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "restart")
	return nil
}

func (h *hooks) probe(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "check")
	return h.check
}

func (h *hooks) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *hooks) Hooks() Hooks { return Hooks{Restart: h.restart, Check: h.probe} }

// flaky fails the first n Gets of a key, so that the retry and wait paths are
// exercised without a sleep in the test.
type flaky struct {
	store.Store
	mu   sync.Mutex
	fail map[string]int
	gets map[string]int
	err  error
}

func newFlaky(s store.Store, err error) *flaky {
	return &flaky{Store: s, fail: map[string]int{}, gets: map[string]int{}, err: err}
}

func (f *flaky) failFor(key string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[key] = n
}

func (f *flaky) Get(ctx context.Context, key string, o store.GetOptions) (*store.Object, error) {
	f.mu.Lock()
	f.gets[key]++
	n := f.fail[key]
	if n > 0 {
		f.fail[key] = n - 1
	}
	f.mu.Unlock()
	if n > 0 {
		return nil, fmt.Errorf("flaky %s: %w", key, f.err)
	}
	return f.Store.Get(ctx, key, o)
}

func (f *flaky) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[key]
}

// sink collects the lifecycle lines so a test can assert on the names
// docs/DESIGN.md 6.6 fixes.
type sink struct {
	mu    sync.Mutex
	lines []logLine
}

type logLine struct {
	level slog.Level
	msg   string
	attr  map[string]string
}

func (s *sink) Enabled(context.Context, slog.Level) bool { return true }

func (s *sink) Handle(_ context.Context, r slog.Record) error {
	l := logLine{level: r.Level, msg: r.Message, attr: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool { l.attr[a.Key] = a.Value.String(); return true })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, l)
	return nil
}

func (s *sink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *sink) WithGroup(string) slog.Handler      { return s }

// has reports a line named msg carrying key=value.
func (s *sink) has(msg, key, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		if l.msg == msg && l.attr[key] == value {
			return true
		}
	}
	return false
}

// count is how many lines carry msg.
func (s *sink) count(msg string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.lines {
		if l.msg == msg {
			n++
		}
	}
	return n
}

func (s *sink) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b bytes.Buffer
	for _, l := range s.lines {
		fmt.Fprintf(&b, "%s %s %v\n", l.level, l.msg, l.attr)
	}
	return b.String()
}
