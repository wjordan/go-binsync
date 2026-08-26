// Package selfupdate is the embedded half of binsync: a service links it,
// takes its listening sockets from it, and is replaced by the next release
// without dropping a connection (README 7, docs/DESIGN.md 6.3).
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"binsync/agent"
	"binsync/release"
)

// The bounds README 7 and docs/DESIGN.md 6.3 fix on a handoff.
const (
	defaultReadyTimeout = 60 * time.Second
	defaultTermTimeout  = 5 * time.Second
	defaultDrainTimeout = 30 * time.Second
)

// Config configures an Updater.
type Config struct {
	// Store is the release stream to poll: s3://, https://, file://.
	Store string
	// Path is the binary to update and to exec. It defaults to os.Args[0]
	// resolved against PATH and the working directory.
	Path string
	// Poll is how often the pointer is fetched; 0 takes the default for the
	// store's scheme (README 5).
	Poll time.Duration
	// Logger receives the lifecycle log (docs/DESIGN.md 6.6); nil is
	// slog.Default.
	Logger *slog.Logger
	// Context stops the updater when it is cancelled, as Stop does.
	Context context.Context
}

// updateSource is the poll -> fetch -> apply -> install half of an update,
// which is binsync/agent. Run drives cycles until ctx is done, calling
// restart once a release is installed at Path; restart hands the sockets
// over and does not return when the new process takes the service over.
type updateSource interface {
	Run(ctx context.Context, restart func(release.Hash) error) error
}

// Updater owns the process lifecycle: the listeners this process serves on,
// the handoff to the next release, and the drain of this one.
type Updater struct {
	log  *slog.Logger
	path string
	inst *release.Installer
	src  updateSource

	ctx    context.Context
	cancel context.CancelFunc

	// The timeouts of the handoff and the two calls that end a process, as
	// fields so that tests can drive the whole lifecycle in milliseconds.
	readyTimeout time.Duration
	termTimeout  time.Duration
	drainTimeout time.Duration
	execve       func(path string, argv, envv []string) error
	exit         func(code int)

	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
	doneOnce  sync.Once

	mu         sync.Mutex
	readyPipe  *os.File
	inherited  []*inheritedFile
	serving    []servedListener
	onShutdown []func()
	upgrading  bool
	superseded bool
}

// servedListener is a listener this process accepts on, keyed by the pair it
// was asked for so that the next process can ask for it the same way.
type servedListener struct {
	ln            net.Listener
	network, addr string
}

// Start begins the update lifecycle: it recovers from an upgrade the previous
// process did not survive (docs/DESIGN.md 6.5), picks up the listeners handed
// over by the process it replaces, and polls the store. It never fails the
// service: a misconfiguration is logged and the process keeps the binary it
// is already running.
func Start(cfg Config) *Updater {
	u := newUpdater(cfg, nil)
	if cfg.Store == "" {
		u.log.Warn("binsync: no store configured; running the lifecycle only")
	} else {
		u.src = &agentSource{agent.Config{Store: cfg.Store, Path: u.path, Poll: cfg.Poll, Logger: u.log}, u.inst}
	}
	u.start()
	return u
}

// newUpdater builds the updater; start is what touches the filesystem, the
// environment and the process.
func newUpdater(cfg Config, src updateSource) *Updater {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	path := cfg.Path
	if path == "" {
		path = selfPath()
	}
	if abs, err := filepath.Abs(path); err == nil {
		// Resolved now: the exec of a handoff much later must not depend on
		// the working directory the service has by then.
		path = abs
	}
	parent := cfg.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)

	return &Updater{
		log:          log,
		path:         path,
		inst:         &release.Installer{Path: path},
		src:          src,
		ctx:          ctx,
		cancel:       cancel,
		readyTimeout: defaultReadyTimeout,
		termTimeout:  defaultTermTimeout,
		drainTimeout: defaultDrainTimeout,
		execve:       syscall.Exec,
		exit:         os.Exit,
		ready:        make(chan struct{}),
		done:         make(chan struct{}),
	}
}

func (u *Updater) start() {
	// Read the handoff environment before anything else can fork: nothing
	// this service starts may inherit a table of descriptors it was not given.
	fdsEnv, readyEnv := os.Getenv(envFDs), os.Getenv(envReady)
	os.Unsetenv(envFDs)
	os.Unsetenv(envReady)

	// Before anything is opened or served: this binary may be a release that
	// crashed on its first run.
	if err := u.selfCheck(fdsEnv != ""); err != nil {
		u.log.Error("binsync: start-up check", "err", err)
	}

	if fdsEnv != "" {
		specs, err := parseFDs(fdsEnv)
		if err != nil {
			u.log.Error("binsync: reading the inherited listeners", "err", err)
		}
		for _, s := range specs {
			name := fmt.Sprintf("binsync %s %s", s.Network, s.Addr)
			u.inherited = append(u.inherited, &inheritedFile{spec: s, f: os.NewFile(uintptr(s.FD), name)})
		}
	}
	if readyEnv != "" {
		fd, err := strconv.Atoi(readyEnv)
		if err != nil {
			u.log.Error("binsync: reading the ready descriptor", "value", readyEnv, "err", err)
		} else {
			u.readyPipe = os.NewFile(uintptr(fd), "binsync ready")
		}
	}

	go func() {
		<-u.ctx.Done()
		u.closeDone()
	}()
	go u.poll()
}

// selfCheck implements docs/DESIGN.md 6.5. A pending marker on a start that
// no handoff launched means the release the previous process installed never
// reported ready: this binary is that release, and it crashed. Put the
// previous one back and exec it.
func (u *Updater) selfCheck(inherited bool) error {
	if inherited {
		// A handoff launched this process; the parent is waiting for Ready
		// and owns the outcome either way.
		return nil
	}
	h, pending, err := u.inst.Pending()
	if !pending {
		return err
	}
	if _, serr := os.Stat(u.inst.OldPath()); serr != nil {
		// There is nothing to go back to, so the marker cannot be acted on;
		// drop it rather than test it again on every start.
		return errors.Join(err, u.inst.ClearPending())
	}
	u.log.Warn("binsync: the installed release did not come up; reverting", "release", h)
	// Record before reverting: a crash between the two must leave the
	// release skipped, not fetched and installed again.
	if merr := u.inst.MarkFailed(h); merr != nil {
		u.log.Error("binsync: recording the failed release", "err", merr)
	}
	if rerr := u.inst.Revert(); rerr != nil {
		return errors.Join(err, rerr)
	}
	return errors.Join(err, u.execve(u.path, os.Args, os.Environ()))
}

// poll runs the update source, but not before this process has declared
// itself healthy: a process that has not called Ready has no business
// replacing the binary its own parent is still serving.
func (u *Updater) poll() {
	select {
	case <-u.ready:
	case <-u.ctx.Done():
		return
	}
	if u.src == nil {
		return
	}
	if err := u.src.Run(u.ctx, u.restart); err != nil && u.ctx.Err() == nil {
		u.log.Error("binsync: the update loop stopped", "err", err)
	}
}

// Listen returns the listener for network and addr: the one handed over by
// the process this one replaced, if that process served the same pair, and a
// fresh one otherwise. The pair is matched as given rather than resolved, so
// both processes must ask for it the same way -- which they do, being two
// builds of the same program.
func (u *Updater) Listen(network, addr string) (net.Listener, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, in := range u.inherited {
		if in.used || in.spec.Network != network || in.spec.Addr != addr {
			continue
		}
		ln, err := net.FileListener(in.f)
		if err != nil {
			return nil, fmt.Errorf("binsync: inheriting the %s listener on %s: %w", network, addr, err)
		}
		// FileListener duplicates the socket; the descriptor it arrived on
		// has no further use in this process.
		in.used = true
		in.f.Close()
		u.serving = append(u.serving, servedListener{ln, network, addr})
		u.log.Info("binsync: inherited a listener", "network", network, "addr", addr)
		return ln, nil
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	u.serving = append(u.serving, servedListener{ln, network, addr})
	return ln, nil
}

// OnShutdown registers a callback to run when this process has been
// superseded and has stopped accepting. The usual body is
// http.Server.Shutdown, plus whatever closes connections Shutdown does not
// track (README 7).
func (u *Updater) OnShutdown(fn func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onShutdown = append(u.onShutdown, fn)
}

// Ready declares this process serving and healthy: it clears the pending
// marker, so a later crash is not read as a failed upgrade, and releases the
// process it replaced. It is the health decision the library makes no attempt
// to second-guess -- do your own checks first -- and it is also what starts
// the update loop. Calling it again does nothing.
func (u *Updater) Ready() {
	u.readyOnce.Do(func() {
		if err := u.inst.ClearPending(); err != nil {
			u.log.Error("binsync: clearing the pending marker", "err", err)
		}
		u.mu.Lock()
		for _, in := range u.inherited {
			if !in.used {
				in.f.Close()
			}
		}
		u.inherited = nil
		pipe := u.readyPipe
		u.readyPipe = nil
		u.mu.Unlock()

		if pipe != nil {
			if _, err := pipe.Write([]byte{1}); err != nil {
				u.log.Error("binsync: reporting ready to the previous process", "err", err)
			}
			pipe.Close()
		}
		close(u.ready)
	})
}

// Done closes when this process has been superseded and its shutdown
// callbacks have run, or when the Config context is cancelled.
func (u *Updater) Done() <-chan struct{} { return u.done }

// Stop ends the update loop and any handoff in flight; the service keeps
// running and Done closes.
func (u *Updater) Stop() { u.cancel() }

func (u *Updater) closeDone() { u.doneOnce.Do(func() { close(u.done) }) }

// selfPath is the path a handoff execs. It comes from os.Args[0] and never
// from /proc/self/exe, which after an install names the replaced inode
// instead of the file now at the path (docs/DESIGN.md D10).
func selfPath() string {
	p, err := exec.LookPath(os.Args[0])
	if err != nil {
		// os.Executable does read /proc/self/exe, but returns the path the
		// process was started from with the kernel's " (deleted)" trimmed.
		if exe, eerr := os.Executable(); eerr == nil {
			return exe
		}
		return os.Args[0]
	}
	return p
}
