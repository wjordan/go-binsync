package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zeebo/blake3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

func init() { Register("ssh", openSSH) }

// sshNotFoundExit is the exit status the remote read command uses for "no
// such object", to tell it apart from a failure of the command itself.
const sshNotFoundExit = 66

// sshDialTimeout bounds the TCP connect and handshake.
const sshDialTimeout = 30 * time.Second

// sshStore publishes into a directory on a remote host over one ssh
// connection. The target polls that same directory through file://, so the
// layout written here is the file backend's layout; the only difference is
// that every write goes through a temp file in the destination directory and
// a rename, so a poller never sees a partial object.
type sshStore struct {
	url    string
	dir    string // remote directory, absolute
	run    sshRunner
	closer io.Closer
}

// sshRunner runs one remote command per call, feeding it stdin and
// collecting stdout. It is an interface so the command strings can be
// exercised against a real shell without a network.
type sshRunner interface {
	Run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error
}

// sshExitError is a non-zero exit status from a remote command.
type sshExitError struct {
	code   int
	stderr string
}

func (e *sshExitError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("remote command exited %d: %s", e.code, e.stderr)
	}
	return fmt.Sprintf("remote command exited %d", e.code)
}

// sshTarget is what a ssh:// URL resolves to before dialling.
type sshTarget struct {
	user string
	addr string // host:port
	dir  string
}

// parseSSHURL resolves ssh://[user@]host[:port]/dir. defaultUser is used
// when the URL names none.
func parseSSHURL(u *url.URL, defaultUser string) (sshTarget, error) {
	var t sshTarget
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return t, fmt.Errorf("store: ssh: %q: a password in the URL is not supported, ssh:// authenticates with a key", u.Redacted())
		}
		t.user = u.User.Username()
	}
	if t.user == "" {
		t.user = defaultUser
	}
	if t.user == "" {
		return t, fmt.Errorf("store: ssh: %q: no user in the URL and $USER is unset", u)
	}
	host := u.Hostname()
	if host == "" {
		return t, fmt.Errorf("store: ssh: %q: no host", u)
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	t.addr = net.JoinHostPort(host, port)
	t.dir = path.Clean(u.Path)
	if t.dir == "" || t.dir == "." || t.dir == "/" {
		return t, fmt.Errorf("store: ssh: %q: needs a directory to publish into", u)
	}
	return t, nil
}

func openSSH(u *url.URL) (Store, error) {
	t, err := parseSSHURL(u, os.Getenv("USER"))
	if err != nil {
		return nil, err
	}
	auth, agentConn, err := sshAuth()
	if err != nil {
		return nil, err
	}
	if agentConn != nil {
		// The agent is only consulted during the handshake.
		defer agentConn.Close()
	}
	hostKey, err := sshHostKeyCallback()
	if err != nil {
		return nil, err
	}
	client, err := ssh.Dial("tcp", t.addr, &ssh.ClientConfig{
		User:            t.user,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         sshDialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("store: ssh: connecting to %s@%s: %w", t.user, t.addr, err)
	}
	return &sshStore{url: u.Redacted(), dir: t.dir, run: sshExec{client}, closer: client}, nil
}

// sshAuth prefers the agent at $SSH_AUTH_SOCK and falls back to the two
// default key files. An encrypted key file is skipped: there is nowhere here
// to ask for a passphrase.
func sshAuth() ([]ssh.AuthMethod, io.Closer, error) {
	var methods []ssh.AuthMethod
	var agentConn io.Closer
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if c, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(c).Signers))
			agentConn = c
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		var signers []ssh.Signer
		for _, name := range []string{"id_ed25519", "id_rsa"} {
			b, err := os.ReadFile(filepath.Join(home, ".ssh", name))
			if err != nil {
				continue
			}
			s, err := ssh.ParsePrivateKey(b)
			if err != nil {
				continue
			}
			signers = append(signers, s)
		}
		if len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
		}
	}
	if len(methods) == 0 {
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, nil, errors.New("store: ssh: no agent at $SSH_AUTH_SOCK and no usable ~/.ssh/id_ed25519 or ~/.ssh/id_rsa")
	}
	return methods, agentConn, nil
}

// sshHostKeyCallback verifies the host against ~/.ssh/known_hosts. An
// unknown host is an error: authenticity of an ssh:// store rests entirely
// on the host key, so there is no accept-anything path.
func sshHostKeyCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("store: ssh: finding the home directory: %w", err)
	}
	file := filepath.Join(home, ".ssh", "known_hosts")
	check, err := knownhosts.New(file)
	if err != nil {
		return nil, fmt.Errorf("store: ssh: reading %s: %w", file, err)
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := check(host, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			return fmt.Errorf("host %s is not in %s: connect once with ssh, or add it with ssh-keyscan", host, file)
		}
		return err
	}, nil
}

func (s *sshStore) URL() string { return s.url }

func (s *sshStore) Close() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// Get reads the whole object and slices a ranged read out of it: there is no
// ranged read over a shell command. Nothing needs one — ssh:// is
// publish-only, and the only object a publisher reads back is the pointer.
func (s *sshStore) Get(ctx context.Context, key string, o GetOptions) (*Object, error) {
	if err := sshCheckKey(key); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := s.run.Run(ctx, sshGetCmd(s.dir, key), nil, &buf); err != nil {
		var exit *sshExitError
		if errors.As(err, &exit) && exit.code == sshNotFoundExit {
			return nil, fmt.Errorf("store: ssh: %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("store: ssh: reading %s: %w", key, err)
	}
	b := buf.Bytes()
	etag := sshETag(b)
	if o.IfNoneMatch != "" && o.IfNoneMatch == etag {
		// The bytes have already crossed the link; this saves the caller
		// work, not bandwidth.
		return nil, fmt.Errorf("store: ssh: %s: %w", key, ErrNotModified)
	}
	if o.Off > 0 || o.Len > 0 {
		off := max(0, min(o.Off, int64(len(b))))
		end := int64(len(b))
		if o.Len > 0 {
			end = min(off+o.Len, end)
		}
		b = b[off:end]
	}
	return &Object{Body: io.NopCloser(bytes.NewReader(b)), Size: int64(len(b)), ETag: etag}, nil
}

// Put streams r into a temp file in the destination directory and renames it
// over key.
//
// PutOptions.IfMatch is a read-then-write, not the atomic compare-and-swap
// S3 gives: two publishers racing on one store can both pass the check and
// the later rename wins. There is no CAS primitive over a plain shell short
// of a lock file, and a store is one release stream with one publisher
// (DESIGN §4.2) — the check is here to catch a pointer that was read before
// someone else's publish, not to arbitrate concurrent publishers.
//
// ContentType and CacheControl are ignored: a directory carries no object
// metadata, and the file:// poller does not look for any.
func (s *sshStore) Put(ctx context.Context, key string, r io.Reader, o PutOptions) error {
	if err := sshCheckKey(key); err != nil {
		return err
	}
	if o.IfMatch != nil {
		cur, err := s.etagOf(ctx, key)
		if err != nil {
			return err
		}
		if cur != *o.IfMatch {
			return fmt.Errorf("store: ssh: writing %s: etag is %q, not %q: %w", key, cur, *o.IfMatch, ErrPreconditionFailed)
		}
	}
	if err := s.run.Run(ctx, sshPutCmd(s.dir, key, sshTempKey(key), o.Size), r, io.Discard); err != nil {
		return fmt.Errorf("store: ssh: writing %s: %w", key, err)
	}
	return nil
}

// etagOf is the current ETag of key, or "" if it does not exist.
func (s *sshStore) etagOf(ctx context.Context, key string) (string, error) {
	obj, err := s.Get(ctx, key, GetOptions{})
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	obj.Body.Close()
	return obj.ETag, nil
}

// sshETag identifies a version of an object by its content: a remote
// directory has no version tag of its own.
func sshETag(b []byte) string {
	h := blake3.Sum256(b)
	return "b3:" + hex.EncodeToString(h[:])
}

// sshTempKey is a hidden sibling of key, so the temp file and its
// destination are in the same directory and the rename is atomic.
func sshTempKey(key string) string {
	return path.Join(path.Dir(key), ".binsync.tmp."+rand.Text())
}

// sshCheckKey rejects any key that could escape the store directory or reach
// the remote shell. Keys come from hashes and a user-supplied prefix and are
// pasted into a command line, so they are treated as untrusted.
func sshCheckKey(key string) error {
	if key == "" {
		return errors.New("store: ssh: the object key is empty")
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("store: ssh: key %q must be relative to the store directory", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("store: ssh: key %q has an empty or relative path element", key)
		}
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_', c == '/':
		default:
			return fmt.Errorf("store: ssh: key %q may not contain %q: a remote path is limited to letters, digits and .-_/", key, key[i:i+1])
		}
	}
	return nil
}

// sshPath is the remote path of key.
func sshPath(dir, key string) string { return path.Join(dir, key) }

// sshQuote wraps s for a POSIX shell.
func sshQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshPutCmd writes stdin to a temp file and renames it over key, so a
// file:// poller in the same directory only ever sees a whole object. When
// size is known the temp file is measured first: a stdin copy that stops
// early — a read error, a half-closed channel — looks to the remote `cat`
// like a clean EOF, and would otherwise publish a truncated object under an
// immutable key. Any failure removes the temp file and exits non-zero.
//
// A size of 0 skips the check along with -1 ("unknown"): an empty object
// cannot be truncated, and PutOptions.Size left at its zero value must not
// mean "must be empty".
func sshPutCmd(dir, key, tmp string, size int64) string {
	dst := sshQuote(sshPath(dir, key))
	t := sshQuote(sshPath(dir, tmp))
	cmd := "mkdir -p " + sshQuote(path.Dir(sshPath(dir, key))) + " && cat > " + t
	if size > 0 {
		cmd += ` && [ "$(wc -c < ` + t + `)" -eq ` + strconv.FormatInt(size, 10) + " ]"
	}
	return cmd + " && mv -f " + t + " " + dst + " || (rm -f " + t + "; exit 1)"
}

// sshGetCmd writes key to stdout, or exits sshNotFoundExit if it is not
// there, which a failure of the command itself would not do.
func sshGetCmd(dir, key string) string {
	p := sshQuote(sshPath(dir, key))
	return "if [ -f " + p + " ]; then cat " + p + "; else exit " + strconv.Itoa(sshNotFoundExit) + "; fi"
}

// sshExec runs each command in its own session on one client connection.
type sshExec struct{ client *ssh.Client }

func (e sshExec) Run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	sess, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("opening a session: %w", err)
	}
	defer sess.Close()
	var stderr bytes.Buffer
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, &stderr

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			sess.Close() // closes the channel, which kills the remote command
		case <-done:
		}
	}()

	err = sess.Run(cmd)
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return &sshExitError{code: exit.ExitStatus(), stderr: strings.TrimSpace(stderr.String())}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}
