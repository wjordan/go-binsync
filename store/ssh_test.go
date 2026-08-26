package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sshShellRunner runs the remote command strings through a local POSIX shell,
// which is what the far end of a session does with them. It keeps the tests
// off the network while still checking the commands themselves.
type sshShellRunner struct{}

func (sshShellRunner) Run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	var stderr bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, &stderr
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &sshExitError{code: exit.ExitCode(), stderr: strings.TrimSpace(stderr.String())}
	}
	return err
}

func sshTestStore(t *testing.T) *sshStore {
	t.Helper()
	dir := t.TempDir()
	return &sshStore{url: "ssh://host" + dir, dir: dir, run: sshShellRunner{}}
}

func sshPut(t *testing.T, s *sshStore, key, body string) {
	t.Helper()
	if err := s.Put(t.Context(), key, strings.NewReader(body), PutOptions{Size: int64(len(body))}); err != nil {
		t.Fatalf("Put(%q) = %v", key, err)
	}
}

func sshGetString(t *testing.T, s *sshStore, key string, o GetOptions) (string, string) {
	t.Helper()
	obj, err := s.Get(t.Context(), key, o)
	if err != nil {
		t.Fatalf("Get(%q) = %v", key, err)
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	if obj.Size != int64(len(b)) {
		t.Errorf("Get(%q).Size = %d, body is %d bytes", key, obj.Size, len(b))
	}
	return string(b), obj.ETag
}

// sshTemps is the temp files left behind anywhere under the store directory.
func sshTemps(t *testing.T, s *sshStore) []string {
	t.Helper()
	var left []string
	err := filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".binsync.tmp.") {
			left = append(left, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return left
}

func TestSSHPutGet(t *testing.T) {
	t.Parallel()
	s := sshTestStore(t)
	ctx := t.Context()

	// A key with a directory component: publish creates patches/ itself.
	sshPut(t, s, "patches/aabbccdd-11223344.bsz", "patch bytes")
	got, etag := sshGetString(t, s, "patches/aabbccdd-11223344.bsz", GetOptions{})
	if got != "patch bytes" {
		t.Errorf("body = %q", got)
	}
	if etag != sshETag([]byte("patch bytes")) {
		t.Errorf("ETag = %q", etag)
	}
	on, err := os.ReadFile(filepath.Join(s.dir, "patches", "aabbccdd-11223344.bsz"))
	if err != nil || string(on) != "patch bytes" {
		t.Errorf("on disk: %q, %v", on, err)
	}

	// Replacing an object.
	sshPut(t, s, "latest.json", "{}")
	sshPut(t, s, "latest.json", "{\"seq\":2}")
	if got, _ := sshGetString(t, s, "latest.json", GetOptions{}); got != "{\"seq\":2}" {
		t.Errorf("after replace: %q", got)
	}

	// Ranged read, and the ETag stays the whole object's.
	body, whole := sshGetString(t, s, "patches/aabbccdd-11223344.bsz", GetOptions{Off: 6, Len: 5})
	if body != "bytes" {
		t.Errorf("ranged body = %q", body)
	}
	if whole != etag {
		t.Errorf("ranged ETag = %q, want the whole object's %q", whole, etag)
	}
	// A range that runs off the end is clamped; Len 0 means "to the end".
	if body, _ := sshGetString(t, s, "latest.json", GetOptions{Off: 1, Len: 99}); body != `{"seq":2}`[1:] {
		t.Errorf("clamped range = %q", body)
	}
	if body, _ := sshGetString(t, s, "latest.json", GetOptions{Off: 6}); body != `:2}` {
		t.Errorf("open-ended range = %q", body)
	}
	if body, _ := sshGetString(t, s, "latest.json", GetOptions{Off: 99, Len: 4}); body != "" {
		t.Errorf("range past the end = %q", body)
	}

	if _, err := s.Get(ctx, "latest.json", GetOptions{IfNoneMatch: sshETag([]byte(`{"seq":2}`))}); !errors.Is(err, ErrNotModified) {
		t.Errorf("IfNoneMatch on the current ETag = %v, want ErrNotModified", err)
	}
	if _, err := s.Get(ctx, "blobs/deadbeef.zst", GetOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a missing key = %v, want ErrNotFound", err)
	}
	if left := sshTemps(t, s); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestSSHPutIfMatch(t *testing.T) {
	t.Parallel()
	s := sshTestStore(t)
	ctx := t.Context()
	want := func(s string) *string { return &s }

	// "" means the object must not exist.
	if err := s.Put(ctx, "latest.json", strings.NewReader("first"), PutOptions{Size: 5, IfMatch: want("")}); err != nil {
		t.Fatalf("create with IfMatch \"\": %v", err)
	}
	etag := sshETag([]byte("first"))

	if err := s.Put(ctx, "latest.json", strings.NewReader("x"), PutOptions{Size: 1, IfMatch: want("")}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("IfMatch \"\" over an existing object = %v, want ErrPreconditionFailed", err)
	}
	if err := s.Put(ctx, "latest.json", strings.NewReader("x"), PutOptions{Size: 1, IfMatch: want("b3:stale")}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("IfMatch on a stale ETag = %v, want ErrPreconditionFailed", err)
	}
	if got, _ := sshGetString(t, s, "latest.json", GetOptions{}); got != "first" {
		t.Errorf("a failed CAS changed the object: %q", got)
	}
	if err := s.Put(ctx, "latest.json", strings.NewReader("second"), PutOptions{Size: 6, IfMatch: &etag}); err != nil {
		t.Fatalf("IfMatch on the current ETag: %v", err)
	}
	if got, _ := sshGetString(t, s, "latest.json", GetOptions{}); got != "second" {
		t.Errorf("after CAS: %q", got)
	}
	if left := sshTemps(t, s); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// A short write must not be renamed over the key: publishing a truncated
// object under an immutable key would be permanent.
func TestSSHPutShortWrite(t *testing.T) {
	t.Parallel()
	s := sshTestStore(t)
	sshPut(t, s, "latest.json", "whole")

	err := s.Put(t.Context(), "latest.json", strings.NewReader("cut"), PutOptions{Size: 12})
	if err == nil {
		t.Fatal("Put of fewer bytes than Size succeeded")
	}
	if got, _ := sshGetString(t, s, "latest.json", GetOptions{}); got != "whole" {
		t.Errorf("object after a short write: %q", got)
	}
	if left := sshTemps(t, s); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestSSHCheckKey(t *testing.T) {
	t.Parallel()
	ok := []string{
		"latest.json",
		"patches/aabbccdd-11223344.bsz",
		"blobs/" + strings.Repeat("ab", 32) + ".zst",
		".binsync.tmp.0011223344556677",
	}
	bad := []string{
		"",
		"/latest.json",
		"../latest.json",
		"patches/../../etc/passwd",
		"patches//x.bsz",
		"patches/",
		"./latest.json",
		"latest.json;rm -rf /",
		"$(id).json",
		"`id`.json",
		"a|b", "a&b", "a b", "a\tb", "a\nb", "a'b", `a"b`, `a\b`, "a*b", "a?b",
		"~/latest.json",
		"café.json",
		"latest.json\x00",
	}
	for _, k := range ok {
		if err := sshCheckKey(k); err != nil {
			t.Errorf("sshCheckKey(%q) = %v, want nil", k, err)
		}
	}
	for _, k := range bad {
		if err := sshCheckKey(k); err == nil {
			t.Errorf("sshCheckKey(%q) = nil, want an error", k)
		}
	}
}

// A rejected key must never reach the shell.
func TestSSHRejectedKeyDoesNotRun(t *testing.T) {
	t.Parallel()
	s := sshTestStore(t)
	s.run = sshRunnerFunc(func(context.Context, string, io.Reader, io.Writer) error {
		t.Error("the runner was reached with an unsafe key")
		return nil
	})
	if err := s.Put(t.Context(), "../escape", strings.NewReader("x"), PutOptions{Size: 1}); err == nil {
		t.Error("Put of an unsafe key succeeded")
	}
	if _, err := s.Get(t.Context(), "$(id)", GetOptions{}); err == nil {
		t.Error("Get of an unsafe key succeeded")
	}
}

type sshRunnerFunc func(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error

func (f sshRunnerFunc) Run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	return f(ctx, cmd, stdin, stdout)
}

func TestParseSSHURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw            string
		defaultUser    string
		user, addr, ln string
		wantErr        bool
	}{
		{raw: "ssh://host/var/lib/app/releases", defaultUser: "will", user: "will", addr: "host:22", ln: "/var/lib/app/releases"},
		{raw: "ssh://deploy@host/srv/app", defaultUser: "will", user: "deploy", addr: "host:22", ln: "/srv/app"},
		{raw: "ssh://deploy@host:2222/srv/app/", defaultUser: "", user: "deploy", addr: "host:2222", ln: "/srv/app"},
		{raw: "ssh://[2001:db8::1]:2222/srv/app", defaultUser: "will", user: "will", addr: "[2001:db8::1]:2222", ln: "/srv/app"},
		{raw: "ssh://host/srv/./app/../app", defaultUser: "will", user: "will", addr: "host:22", ln: "/srv/app"},
		{raw: "ssh://host/srv/app", defaultUser: "", wantErr: true},          // no user anywhere
		{raw: "ssh://host", defaultUser: "will", wantErr: true},              // no directory
		{raw: "ssh://host/", defaultUser: "will", wantErr: true},             // the remote root is not a store
		{raw: "ssh:///srv/app", defaultUser: "will", wantErr: true},          // no host
		{raw: "ssh://u:pw@host/srv/app", defaultUser: "will", wantErr: true}, // passwords are not supported
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", c.raw, err)
		}
		got, err := parseSSHURL(u, c.defaultUser)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSSHURL(%q) = %+v, want an error", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSSHURL(%q) = %v", c.raw, err)
			continue
		}
		if got.user != c.user || got.addr != c.addr || got.dir != c.ln {
			t.Errorf("parseSSHURL(%q) = %+v, want {%s %s %s}", c.raw, got, c.user, c.addr, c.ln)
		}
	}
}

func TestSSHQuote(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/srv/app":     "'/srv/app'",
		"":             "''",
		"a b;c":        "'a b;c'",
		`/srv/o'brien`: `'/srv/o'\''brien'`,
	}
	for in, want := range cases {
		if got := sshQuote(in); got != want {
			t.Errorf("sshQuote(%q) = %s, want %s", in, got, want)
		}
	}
	// The quoting has to survive a shell that would otherwise interpret it.
	for in := range cases {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+sshQuote(in)).Output()
		if err != nil || string(out) != in {
			t.Errorf("sh printf %s = %q, %v", sshQuote(in), out, err)
		}
	}
}

func TestSSHCommands(t *testing.T) {
	t.Parallel()
	const tmp = ".binsync.tmp.0011223344556677"
	got := sshPutCmd("/srv/app", "patches/aa-bb.bsz", "patches/"+tmp, 42)
	want := `mkdir -p '/srv/app/patches' && cat > '/srv/app/patches/` + tmp + `'` +
		` && [ "$(wc -c < '/srv/app/patches/` + tmp + `')" -eq 42 ]` +
		` && mv -f '/srv/app/patches/` + tmp + `' '/srv/app/patches/aa-bb.bsz'` +
		` || (rm -f '/srv/app/patches/` + tmp + `'; exit 1)`
	if got != want {
		t.Errorf("sshPutCmd:\n got %s\nwant %s", got, want)
	}

	// An unknown or empty size drops the length check; the directory is
	// still quoted.
	if zero := sshPutCmd("/srv/app", "latest.json", tmp, 0); strings.Contains(zero, "wc -c") {
		t.Errorf("a zero size still measures the temp file: %s", zero)
	}
	got = sshPutCmd(`/srv/o'brien`, "latest.json", tmp, -1)
	want = `mkdir -p '/srv/o'\''brien' && cat > '/srv/o'\''brien/` + tmp + `'` +
		` && mv -f '/srv/o'\''brien/` + tmp + `' '/srv/o'\''brien/latest.json'` +
		` || (rm -f '/srv/o'\''brien/` + tmp + `'; exit 1)`
	if got != want {
		t.Errorf("sshPutCmd, unknown size:\n got %s\nwant %s", got, want)
	}

	got = sshGetCmd("/srv/app", "latest.json")
	want = `if [ -f '/srv/app/latest.json' ]; then cat '/srv/app/latest.json'; else exit 66; fi`
	if got != want {
		t.Errorf("sshGetCmd:\n got %s\nwant %s", got, want)
	}
}

func TestSSHTempKeyIsASibling(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"latest.json", "patches/aa-bb.bsz"} {
		tmp := sshTempKey(key)
		if filepath.Dir(tmp) != filepath.Dir(key) {
			t.Errorf("sshTempKey(%q) = %q, want a sibling", key, tmp)
		}
		if err := sshCheckKey(tmp); err != nil {
			t.Errorf("sshTempKey(%q) = %q, which is not a safe key: %v", key, tmp, err)
		}
		if other := sshTempKey(key); other == tmp {
			t.Errorf("sshTempKey(%q) repeated %q", key, tmp)
		}
	}
}

func TestSSHRegistered(t *testing.T) {
	t.Parallel()
	mu.RLock()
	open := openers["ssh"]
	mu.RUnlock()
	if open == nil {
		t.Fatal("the ssh scheme is not registered")
	}
	// Resolving a URL must fail before any dial when the URL is malformed.
	if _, err := open(&url.URL{Scheme: "ssh", Host: "host"}); err == nil {
		t.Error("a URL with no directory opened")
	}
}
