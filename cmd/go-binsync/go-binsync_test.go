package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wjordan/go-binsync/agent"
	"github.com/wjordan/go-binsync/release"
	"github.com/wjordan/go-binsync/store"
)

// fill writes a deterministic pseudo-random stream, so a test binary is
// incompressible and reproducible without a fixture file.
func fill(b []byte, seed uint64) {
	// The state must not be flattened (an `| 1` would make consecutive seeds
	// collide); xorshift only needs it to be non-zero.
	x := seed*0x9e3779b97f4a7c15 + 0x9e3779b9
	if x == 0 {
		x = 1
	}
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x)
	}
}

// fakeRelease is a stand-in for a built binary: the plain codec is what these
// tests exercise, and it neither knows nor cares that this is not an ELF.
func fakeRelease(seed uint64, n int) []byte {
	b := make([]byte, n)
	fill(b, seed)
	return b
}

// edited is the shape of a real release: most of the file survives, one
// region is rewritten, and everything after it shifts.
func edited(base []byte, seed uint64) []byte {
	out := make([]byte, 0, len(base)+64)
	cut := len(base) * 2 / 5
	out = append(out, base[:cut]...)
	patchIn := make([]byte, 4096+64)
	fill(patchIn, seed)
	out = append(out, patchIn...)
	return append(out, base[cut+4096:]...)
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return newLogger(testWriter{t})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(b, "\n"))
	return len(b), nil
}

func write(t *testing.T, path string, b []byte) string {
	t.Helper()
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// mustPublish publishes and returns the head the store then names, so a test
// keeps the blob table of a release the pointer will later forget.
func mustPublish(t *testing.T, log *slog.Logger, cache, bin, storeURL string) release.Release {
	t.Helper()
	if err := publish(t.Context(), log, []string{"--cache", cache, bin, storeURL}); err != nil {
		t.Fatalf("publish %s: %v", bin, err)
	}
	return readPointer(t, storeURL[len("file://"):]).Head
}

func readPointer(t *testing.T, dir string) *release.Pointer {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, release.PointerKey))
	if err != nil {
		t.Fatal(err)
	}
	p, err := release.ParsePointer(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEndToEnd is the whole product over a file:// store: three releases
// published, then a target two releases behind brought to the head by the
// chain, restarted and checked.
func TestEndToEnd(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	storeURL := "file://" + storeDir

	v1 := fakeRelease(1, 200<<10)
	v2 := edited(v1, 2)
	v3 := edited(v2, 3)
	p1 := write(t, filepath.Join(dir, "v1"), v1)
	p2 := write(t, filepath.Join(dir, "v2"), v2)
	p3 := write(t, filepath.Join(dir, "v3"), v3)

	warm := filepath.Join(dir, "cache")
	heads := []release.Release{
		mustPublish(t, log, warm, p1, storeURL),
		mustPublish(t, log, warm, p2, storeURL),
		// A cold cache is the one case where the publisher fetches the
		// previous release's blob back out of the store to encode against.
		mustPublish(t, log, filepath.Join(dir, "cold"), p3, storeURL),
	}

	h1, h2, h3 := release.HashBytes(v1), release.HashBytes(v2), release.HashBytes(v3)
	p := readPointer(t, storeDir)
	if p.Head.Hash != h3 || p.Head.Size != int64(len(v3)) {
		t.Fatalf("head is %s/%d, want %s/%d", p.Head.Hash, p.Head.Size, h3, len(v3))
	}
	if len(p.Chain) != 2 || p.Chain[0].From != h2 || p.Chain[1].From != h1 {
		t.Fatalf("chain is %+v", p.Chain)
	}
	for _, e := range p.Chain {
		b, err := os.ReadFile(filepath.Join(storeDir, e.Key))
		if err != nil || int64(len(b)) != e.Size || release.HashBytes(b) != e.B3 {
			t.Fatalf("patch %s: %d bytes, err %v", e.Key, len(b), err)
		}
		if e.Size >= p.Head.Blob.Size {
			t.Fatalf("patch %s (%d) is not smaller than the blob (%d)", e.Key, e.Size, p.Head.Blob.Size)
		}
	}

	// Every release keeps a blob, and each one reconstructs its release.
	s, err := store.Open(storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, want := range [][]byte{v1, v2, v3} {
		got, err := fetchBlob(t.Context(), s, heads[i])
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("blob of %s: %v", heads[i].Hash, err)
		}
	}

	// A target two releases behind follows the chain to the head.
	target := write(t, filepath.Join(dir, "app"), v1)
	marker := filepath.Join(dir, "restarted")
	err = agentCmd(t.Context(), log, []string{
		"--once", "--restart", "touch " + marker, "--healthy", "test -f " + marker, storeURL, target,
	})
	if err != nil {
		t.Fatalf("agent --once: %v", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, v3) {
		t.Fatal("the target is not the head release")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("--restart did not run")
	}
	inst := &release.Installer{Path: target}
	if _, pending, _ := inst.Pending(); pending {
		t.Fatal("the pending marker survived a healthy update")
	}
	if old, _ := os.ReadFile(inst.OldPath()); !bytes.Equal(old, v1) {
		t.Fatal(".old is not the release that was replaced")
	}

	// A second cycle has nothing to do and says so.
	if err := agentCmd(t.Context(), log, []string{"--once", "--restart", "false", storeURL, target}); err != nil {
		t.Fatalf("agent --once at head: %v", err)
	}
}

// TestPublishIsIdempotent proves the second publish of the same binary
// changes nothing at all.
func TestPublishIsIdempotent(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	storeURL := "file://" + storeDir
	cache := filepath.Join(dir, "cache")

	bin := write(t, filepath.Join(dir, "v1"), fakeRelease(7, 64<<10))
	mustPublish(t, log, cache, bin, storeURL)
	before := readPointer(t, storeDir)
	names := listStore(t, storeDir)

	mustPublish(t, log, cache, bin, storeURL)
	after := readPointer(t, storeDir)
	if after.Seq != before.Seq || after.Head.Hash != before.Head.Hash {
		t.Fatalf("the pointer moved: seq %d -> %d", before.Seq, after.Seq)
	}
	if got := listStore(t, storeDir); len(got) != len(names) {
		t.Fatalf("the store gained objects: %v -> %v", names, got)
	}
}

// TestPublishBlobOnly covers the rule that a patch is never published unless
// it is smaller than the blob: two unrelated releases have no patch worth
// sending.
func TestPublishBlobOnly(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	storeURL := "file://" + storeDir
	cache := filepath.Join(dir, "cache")

	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v1"), fakeRelease(11, 128<<10)), storeURL)
	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v2"), fakeRelease(12, 128<<10)), storeURL)

	p := readPointer(t, storeDir)
	if len(p.Chain) != 0 {
		t.Fatalf("chain is %+v, want empty: the patch is not smaller than the blob", p.Chain)
	}
	for _, name := range listStore(t, storeDir) {
		if filepath.Dir(name) == "patches" {
			t.Fatalf("a patch was published anyway: %s", name)
		}
	}

	// The target has no chain to follow, so it takes the blob.
	target := write(t, filepath.Join(dir, "app"), fakeRelease(11, 128<<10))
	if err := agentCmd(t.Context(), log, []string{"--once", "--restart", "true", storeURL, target}); err != nil {
		t.Fatalf("agent --once: %v", err)
	}
	want, _ := os.ReadFile(filepath.Join(dir, "v2"))
	if got, _ := os.ReadFile(target); !bytes.Equal(got, want) {
		t.Fatal("the target did not take the blob")
	}
}

// TestAgentRollsBack: --restart fails, so the previous binary comes back,
// the release is recorded as failed and skipped, and the exit code is 5.
func TestAgentRollsBack(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	storeURL := "file://" + storeDir
	cache := filepath.Join(dir, "cache")

	v1 := fakeRelease(21, 64<<10)
	v2 := edited(v1, 22)
	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v1"), v1), storeURL)
	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v2"), v2), storeURL)

	target := write(t, filepath.Join(dir, "app"), v1)
	restarts := filepath.Join(dir, "restarts")
	args := []string{"--once", "--restart", "echo x >> " + restarts + "; false", storeURL, target}

	err := agentCmd(t.Context(), log, args)
	var e *exitError
	if !errors.As(err, &e) || e.code != codeRolledBack {
		t.Fatalf("got %v, want exit %d", err, codeRolledBack)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, v1) {
		t.Fatal("the previous binary did not come back")
	}
	if failed, ok := (&release.Installer{Path: target}).Failed(); !ok || failed != release.HashBytes(v2) {
		t.Fatalf("failed marker is %s/%v", failed, ok)
	}
	if b, _ := os.ReadFile(restarts); len(bytes.Fields(b)) != 2 {
		t.Fatalf("--restart ran %d times, want 2 (the new release and the revert)", len(bytes.Fields(b)))
	}

	// The next cycle must not try the release that just failed.
	if err := agentCmd(t.Context(), log, args); err != nil {
		t.Fatalf("the failed release was retried: %v", err)
	}
	if b, _ := os.ReadFile(restarts); len(bytes.Fields(b)) != 2 {
		t.Fatal("the skipped release was restarted anyway")
	}
}

// TestAgentVerifyFailed is README exit code 3: the store served bytes that do
// not match the hashes the pointer carries.
func TestAgentVerifyFailed(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	storeURL := "file://" + storeDir
	cache := filepath.Join(dir, "cache")

	v1 := fakeRelease(61, 64<<10)
	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v1"), v1), storeURL)

	// Flip a byte inside the blob, keeping its length, so only the frame
	// hash can catch it.
	blob := filepath.Join(storeDir, release.BlobKey(release.HashBytes(v1)))
	b, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	write(t, blob, b)

	target := write(t, filepath.Join(dir, "app"), fakeRelease(62, 4096))
	err = agentCmd(t.Context(), log, []string{"--once", "--restart", "true", storeURL, target})
	var e *exitError
	if !errors.As(err, &e) || e.code != codeVerify {
		t.Fatalf("got %v, want exit %d", err, codeVerify)
	}
	if got, _ := os.ReadFile(target); bytes.Equal(got, v1) {
		t.Fatal("corrupt bytes were installed")
	}
}

// TestAgentNoPathToHead is README exit code 4: the pointer names a head this
// target cannot reach by any published object.
func TestAgentNoPathToHead(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &release.Pointer{
		Format: release.Format,
		Seq:    1,
		Head:   release.Release{Hash: release.HashBytes([]byte("a release nobody published")), Size: 10},
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(storeDir, release.PointerKey), b)
	target := write(t, filepath.Join(dir, "app"), fakeRelease(31, 4096))

	err = agentCmd(t.Context(), log, []string{"--once", "--restart", "true", "file://" + storeDir, target})
	var e *exitError
	if !errors.As(err, &e) || e.code != codeNoPath {
		t.Fatalf("got %v, want exit %d", err, codeNoPath)
	}
}

// TestPublishFinishesAnUnfinishedBlob covers the D13 recovery: a publisher
// that died between the pointer and the blob leaves a head that 404s, and the
// next publish is what heals it.
func TestPublishFinishesAnUnfinishedBlob(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "store")
	storeURL := "file://" + storeDir
	cache := filepath.Join(dir, "cache")

	v1 := fakeRelease(41, 64<<10)
	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v1"), v1), storeURL)
	blobPath := filepath.Join(storeDir, release.BlobKey(release.HashBytes(v1)))
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}

	v2 := edited(v1, 42)
	mustPublish(t, log, cache, write(t, filepath.Join(dir, "v2"), v2), storeURL)
	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("the previous head's blob was not restored: %v", err)
	}
	want, _ := release.EncodeBlob(release.HashBytes(v1), v1)
	if !bytes.Equal(got, want) {
		t.Fatal("the restored blob is not the object the pointer describes")
	}
}

func listStore(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel != ".go-binsync.lock" {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiffAndPatch(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	dir := t.TempDir()
	v1 := fakeRelease(51, 64<<10)
	v2 := edited(v1, 52)
	p1 := write(t, filepath.Join(dir, "v1"), v1)
	p2 := write(t, filepath.Join(dir, "v2"), v2)
	patchPath := filepath.Join(dir, "p.bsz")

	if err := diff(log, []string{"-v", "-o", patchPath, p1, p2}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rebuilt")
	if err := patchCmd(log, []string{"-o", out, p1, patchPath}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, v2) {
		t.Fatal("patch did not reproduce the new release")
	}

	// A truncated patch is a verification failure, not a bad file.
	b, _ := os.ReadFile(patchPath)
	write(t, patchPath, b[:len(b)/2])
	err := patchCmd(log, []string{"-o", filepath.Join(dir, "nope"), p1, patchPath})
	var e *exitError
	if !errors.As(err, &e) || e.code != codeVerify {
		t.Fatalf("got %v, want exit %d", err, codeVerify)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	log := testLogger(t)
	for _, args := range [][]string{
		nil,
		{"bogus"},
		{"publish", "only-one-arg"},
		{"agent", "file:///tmp", "/tmp/x"}, // no --restart
		{"diff", "a", "b"},                 // no -o
		{"patch", "a", "b"},
	} {
		err := run(t.Context(), log, args)
		var e *exitError
		if !errors.As(err, &e) || e.code != codeUsage {
			t.Errorf("%v: got %v, want exit %d", args, err, codeUsage)
		}
	}
}

func TestReleaseCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := openCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = cacheLimit + 5
	var hashes []release.Hash
	now := time.Now()
	for i := range n {
		b := fakeRelease(uint64(i)+100, 512)
		h := release.HashBytes(b)
		if err := c.put(h, b); err != nil {
			t.Fatal(err)
		}
		// The LRU is keyed on mtime; make the order unambiguous without
		// making any entry look newer than the one that follows it.
		age := time.Duration(n-i) * time.Second
		os.Chtimes(c.path(h), now, now.Add(-age))
		hashes = append(hashes, h)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != cacheLimit {
		t.Fatalf("cache holds %d entries, want %d", len(ents), cacheLimit)
	}
	for i, h := range hashes {
		if got, want := c.get(h) != nil, i >= n-cacheLimit; got != want {
			t.Errorf("entry %d present = %v, want %v", i, got, want)
		}
	}

	// A corrupt entry costs a download, never a wrong patch.
	last := hashes[n-1]
	write(t, c.path(last), []byte("not the release"))
	if got := c.get(last); got != nil {
		t.Fatal("a corrupt cache entry was served")
	}
	if got := c.get(release.HashBytes([]byte("never cached"))); got != nil {
		t.Fatal("a miss returned bytes")
	}
}

// TestExitCodes pins the two independent encodings of README 5's table
// against each other: the agent decides the code, the CLI documents it.
func TestExitCodes(t *testing.T) {
	t.Parallel()
	for o, want := range map[agent.Outcome]int{
		agent.OutcomeAtHead:       codeOK,
		agent.OutcomeUpdated:      codeOK,
		agent.OutcomeError:        codeError,
		agent.OutcomeVerifyFailed: codeVerify,
		agent.OutcomeNoPath:       codeNoPath,
		agent.OutcomeRolledBack:   codeRolledBack,
	} {
		if got := o.ExitCode(); got != want {
			t.Errorf("%s exits %d, want %d", o, got, want)
		}
	}
}

func TestHumanFormatting(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"}, {999, "999 B"}, {1000, "1.0 KB"}, {111552, "112 KB"},
		{999999, "1.0 MB"}, {2_100_000, "2.1 MB"}, {94 << 20, "99 MB"},
		{1_000_000_000, "1.0 GB"},
	} {
		if got := hbytes(c.n); got != c.want {
			t.Errorf("hbytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{2100000001 * time.Nanosecond, "2.1s"},
		{340*time.Millisecond + 400*time.Microsecond, "340ms"},
		{1500 * time.Nanosecond, "2µs"},
	} {
		if got := hdur(c.d); got != c.want {
			t.Errorf("hdur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
