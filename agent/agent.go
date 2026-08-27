// Package agent is go-binsync's update loop: poll the pointer, work out the
// cheapest way to reach the head release, fetch and apply it, install it
// atomically, restart the service and check that it came up — or put the
// previous binary back if it did not.
//
// One loop drives both target shapes (docs/DESIGN.md 2). For the external
// `go-binsync agent` the hooks are the user's --restart and --healthy commands;
// for the embedded go-binsync/selfupdate, Restart is the exec handoff.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/wjordan/go-binsync/release"
	"github.com/wjordan/go-binsync/store"
)

// DefaultMaxSize bounds the release a target is willing to allocate for. The
// pointer is untrusted input to that allocation (docs/DESIGN.md 8).
const DefaultMaxSize = 2 << 30

// The schedule of one cycle. All but maxPoll are Config-independent because
// README 5 and docs/DESIGN.md 6.4 fix them; they are agent fields rather than
// constants only so that tests run in milliseconds.
const (
	maxPoll           = 5 * time.Minute
	defaultCheckFor   = time.Minute
	defaultCheckEvery = time.Second
	defaultBlobWait   = 30 * time.Minute
	defaultBlobRetry  = time.Second
	defaultRetry      = 200 * time.Millisecond
)

// Config is what a target needs to keep one binary up to date.
type Config struct {
	// Store is the release stream URL: s3://, https://, file:// (README 2).
	Store string
	// Path is the binary to update. State that outlives one update — the
	// hash cache and the failed marker — lives in <Path>.go-binsync/.
	Path string
	// Poll is how often the pointer is fetched. 0 takes the default for the
	// store's scheme: 1s for file://, 5s for anything remote (README 5).
	Poll time.Duration
	// Logger receives the per-cycle lifecycle lines (docs/DESIGN.md 6.6).
	// nil is slog.Default.
	Logger *slog.Logger
	// MaxSize refuses a pointer naming a release larger than this. 0 is
	// DefaultMaxSize.
	MaxSize int64
}

// Hooks are the only things the loop does to the service itself.
type Hooks struct {
	// Restart makes the service run the binary that was just installed. A
	// nil Restart installs and does nothing else. In embedded mode this is
	// the exec handoff, which takes the service over and does not return.
	// The loop imposes no deadline: only the hook knows what its restart
	// costs.
	Restart func(ctx context.Context) error
	// Check probes the restarted service once. The loop calls it every
	// second until it returns nil or a minute has passed
	// (docs/DESIGN.md 6.4). A nil Check makes a Restart that returned nil
	// the whole success contract — the deliberate minimum of README 5.
	Check func(ctx context.Context) error
}

// Outcome is how one cycle ended. ExitCode is what `go-binsync agent --once`
// returns (README 5).
type Outcome int

const (
	// OutcomeAtHead means there was nothing to do: the target already runs
	// the head release, or the pointer has not moved.
	OutcomeAtHead Outcome = iota
	// OutcomeUpdated means the head release is installed, restarted and
	// healthy.
	OutcomeUpdated
	// OutcomeError means the cycle failed somewhere a retry can fix.
	OutcomeError
	// OutcomeVerifyFailed means fetched bytes did not match the hashes the
	// pointer carries, so nothing was installed.
	OutcomeVerifyFailed
	// OutcomeNoPath means neither the chain nor a blob reaches this target.
	OutcomeNoPath
	// OutcomeRolledBack means the new release was installed but did not come
	// up, so the previous one is back.
	OutcomeRolledBack
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAtHead:
		return "at-head"
	case OutcomeUpdated:
		return "updated"
	case OutcomeVerifyFailed:
		return "verify-failed"
	case OutcomeNoPath:
		return "no-path"
	case OutcomeRolledBack:
		return "rolled-back"
	}
	return "error"
}

// ExitCode is README 5's table: 0 ok · 1 error · 3 verification failed ·
// 4 no path to head · 5 rolled back.
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeAtHead, OutcomeUpdated:
		return 0
	case OutcomeVerifyFailed:
		return 3
	case OutcomeNoPath:
		return 4
	case OutcomeRolledBack:
		return 5
	}
	return 1
}

// retryable reports whether re-reading the same pointer could end
// differently. It decides both the poll backoff and whether the pointer's
// ETag is worth caching.
func (o Outcome) retryable() bool {
	return o == OutcomeError || o == OutcomeVerifyFailed
}

// Loop polls until ctx is done. A failing cycle is logged and retried with
// the interval doubling to maxPoll, never returned: a target whose store is
// unreachable keeps serving what it has. The error is for a configuration
// the loop cannot start with at all.
func Loop(ctx context.Context, cfg Config, h Hooks) error {
	a, err := newAgent(cfg, h)
	if err != nil {
		return err
	}
	defer a.st.Close()
	a.run(ctx)
	return nil
}

// Once runs a single cycle. It is `go-binsync agent --once`, and the error
// explains any Outcome that is not AtHead or Updated.
func Once(ctx context.Context, cfg Config, h Hooks) (Outcome, error) {
	a, err := newAgent(cfg, h)
	if err != nil {
		return OutcomeError, err
	}
	defer a.st.Close()
	return a.cycle(ctx)
}

// agent is one target: one path, one store, one update at a time (README
// guarantee 8 — the cycle is sequential by construction).
type agent struct {
	hooks Hooks
	log   *slog.Logger
	st    store.Store
	inst  *release.Installer
	path  string
	max   int64
	poll  time.Duration

	// What the last accepted pointer said, so that a replayed one is
	// ignored and an unchanged one costs one round trip and no bytes.
	etag string
	seq  int64
	// skipped is the failed release already logged about, so that a target
	// parked on a bad head does not repeat itself every poll.
	skipped release.Hash

	checkFor, checkEvery time.Duration
	// blobWait is the whole budget for a blob the publisher has not finished
	// uploading; blobRetry is the first wait, doubling to a minute
	// (docs/DESIGN.md 4.4). retry is the first wait between attempts at one
	// object that is there.
	blobWait, blobRetry, retry time.Duration
}

func newAgent(cfg Config, h Hooks) (*agent, error) {
	if cfg.Path == "" {
		return nil, errors.New("agent: no path to keep up to date")
	}
	path, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("agent: %s: %w", cfg.Path, err)
	}
	st, err := store.Open(cfg.Store)
	if err != nil {
		return nil, err
	}
	a := &agent{
		hooks:      h,
		log:        cfg.Logger,
		st:         st,
		inst:       &release.Installer{Path: path},
		path:       path,
		max:        cfg.MaxSize,
		poll:       cfg.Poll,
		checkFor:   defaultCheckFor,
		checkEvery: defaultCheckEvery,
		blobWait:   defaultBlobWait,
		blobRetry:  defaultBlobRetry,
		retry:      defaultRetry,
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.max <= 0 {
		a.max = DefaultMaxSize
	}
	if a.poll <= 0 {
		a.poll = defaultPoll(cfg.Store)
	}
	return a, nil
}

// defaultPoll: a local directory costs a stat, so it can be watched closely;
// a remote store costs a round trip somebody pays for (README 5).
func defaultPoll(raw string) time.Duration {
	if u, err := url.Parse(raw); err == nil && u.Scheme == "file" {
		return time.Second
	}
	return 5 * time.Second
}

func (a *agent) run(ctx context.Context) {
	for fails := 0; ; {
		o, _ := a.cycle(ctx)
		if ctx.Err() != nil {
			return
		}
		if o.retryable() {
			fails++
		} else {
			fails = 0
		}
		wait := a.poll
		for i := 0; i < fails && wait < maxPoll; i++ {
			wait *= 2
		}
		if !sleep(ctx, min(wait, maxPoll)) {
			return
		}
	}
}

// cycle is one poll → plan → fetch → apply → install → restart → check.
func (a *agent) cycle(ctx context.Context) (Outcome, error) {
	o, err := a.update(ctx)
	if o.retryable() {
		// Re-read the pointer next time. Caching its ETag through a failure
		// would answer the next poll with 304 and leave the target sitting
		// on the release this cycle could not install.
		a.etag = ""
	}
	return o, err
}

func (a *agent) update(ctx context.Context) (Outcome, error) {
	// README guarantee 4: know what is on disk before fetching anything, so
	// that a drifted file goes straight to the blob instead of being patched.
	current, err := a.currentHash()
	if err != nil {
		a.log.Error("poll", "err", err)
		return OutcomeError, err
	}

	p, fresh, err := a.pointer(ctx)
	if err != nil {
		a.log.Error("poll", "err", err)
		return OutcomeError, err
	}
	if !fresh {
		return OutcomeAtHead, nil
	}
	if p.Head.Size > a.max {
		err := fmt.Errorf("agent: release %s is %d bytes, over the %d limit", p.Head.Hash, p.Head.Size, a.max)
		a.log.Error("plan", "err", err)
		return OutcomeError, err
	}
	if skip, err := a.skip(p.Head.Hash); skip {
		return OutcomeAtHead, err
	}

	plan := release.MakePlan(p, current)
	if plan.AtHead() {
		a.log.Debug("plan", "kind", plan.Kind.String(), "at_head", true)
		return OutcomeAtHead, nil
	}
	a.log.Info("plan", "kind", plan.Kind.String(), "bytes", plan.Bytes, "edges", len(plan.Edges),
		"from", current.String(), "to", plan.Head.String(), "version", p.Head.Version)
	if plan.Kind == release.PlanNone {
		err := fmt.Errorf("agent: nothing published reaches %s at %s", current, a.path)
		a.log.Error("plan", "err", err)
		return OutcomeNoPath, err
	}

	data, o, err := a.build(ctx, p, plan)
	if err != nil {
		a.log.Error("fetch", "err", err)
		return o, err
	}
	// The last check before the bytes can become the service (README
	// guarantee 1). Install repeats it; getting the outcome right is worth
	// one pass over a file this cycle has already rewritten several times.
	if got := release.HashBytes(data); got != p.Head.Hash {
		err := fmt.Errorf("agent: the assembled release hashes to %s, the pointer says %s: %w", got, p.Head.Hash, errVerify)
		a.log.Error("apply", "err", err)
		return OutcomeVerifyFailed, err
	}

	if err := a.inst.Install(data, p.Head.Hash); err != nil {
		a.log.Error("install", "err", err)
		return OutcomeError, err
	}
	a.log.Info("install", "release", p.Head.Hash.String(), "version", p.Head.Version, "bytes", len(data))

	if a.hooks.Restart != nil {
		a.log.Info("restart", "release", p.Head.Hash.String())
		if err := a.hooks.Restart(ctx); err != nil {
			return a.rollback(ctx, p.Head.Hash, fmt.Errorf("agent: restart: %w", err))
		}
	}
	if err := a.healthy(ctx); err != nil {
		return a.rollback(ctx, p.Head.Hash, err)
	}
	if err := a.inst.ClearPending(); err != nil {
		a.log.Error("install", "err", err)
	}
	return OutcomeUpdated, nil
}

// currentHash is what this target is running. A path that does not exist yet
// is the first install on this host: the zero hash is on no chain edge, so
// the plan is the blob.
func (a *agent) currentHash() (release.Hash, error) {
	h, err := release.CachedHash(a.path)
	if errors.Is(err, fs.ErrNotExist) {
		return release.Hash{}, nil
	}
	return h, err
}

// pointer fetches latest.json conditionally. fresh is false when there is
// nothing to act on: the store answered 304, the pointer is a replay, or it
// was written by a newer publisher than this build understands.
func (a *agent) pointer(ctx context.Context) (*release.Pointer, bool, error) {
	obj, err := a.st.Get(ctx, release.PointerKey, store.GetOptions{IfNoneMatch: a.etag})
	if errors.Is(err, store.ErrNotModified) {
		a.log.Debug("poll", "changed", false)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("agent: poll %s: %w", a.st.URL(), err)
	}
	raw, etag, err := readObject(obj, a.max)
	if err != nil {
		return nil, false, fmt.Errorf("agent: poll %s: %w", a.st.URL(), err)
	}

	p, err := release.ParsePointer(raw)
	if err != nil {
		var unknown *release.UnknownFormatError
		if !errors.As(err, &unknown) {
			return nil, false, err
		}
		// Store format bumps are forward-only: keep the current binary and
		// stop re-reading a pointer this build cannot act on
		// (docs/DESIGN.md 4.2).
		a.log.Warn("poll", "err", err)
		a.etag = etag
		return nil, false, nil
	}
	if p.Seq < a.seq {
		// README guarantee 2: a stale cache or a rolled-back bucket can hold
		// a target back, not move it anywhere. An equal seq is accepted —
		// it is the pointer already in hand, and re-running the cycle on it
		// is how a transient failure is retried.
		a.log.Warn("poll", "ignored", "the pointer is older than the last one", "seq", p.Seq, "last", a.seq)
		a.etag = etag
		return nil, false, nil
	}
	p.ETag, a.etag, a.seq = etag, etag, p.Seq
	a.log.Info("poll", "changed", true, "seq", p.Seq, "head", p.Head.Hash.String(), "version", p.Head.Version)
	return p, true, nil
}

// skip implements README guarantee 7: a release that was installed and
// rolled back is not tried again until the pointer names a different head.
func (a *agent) skip(head release.Hash) (bool, error) {
	failed, ok := a.inst.Failed()
	if !ok {
		return false, nil
	}
	if failed == head {
		if a.skipped != head {
			a.skipped = head
			a.log.Warn("failed", "release", head.String(), "skipped_until", "the pointer moves")
		}
		return true, fmt.Errorf("agent: release %s was rolled back on this target", head)
	}
	a.skipped = release.Hash{}
	if err := a.inst.ClearFailed(); err != nil {
		a.log.Error("failed", "err", err)
	}
	return false, nil
}

// healthy runs Hooks.Check on the schedule docs/DESIGN.md 6.4 fixes: once a
// second until it passes or a minute has gone.
func (a *agent) healthy(ctx context.Context) error {
	if a.hooks.Check == nil {
		return nil
	}
	deadline := time.Now().Add(a.checkFor)
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithDeadline(ctx, deadline)
		err := a.hooks.Check(cctx)
		cancel()
		if err == nil {
			a.log.Info("healthy", "attempts", attempt)
			return nil
		}
		if !sleep(ctx, a.checkEvery) || !time.Now().Before(deadline) {
			return fmt.Errorf("agent: %s was not healthy within %s: %w", a.path, a.checkFor, err)
		}
	}
}

// rollback undoes a release the target could not run. The release is
// recorded before the revert, so that a crash between the two leaves it
// skipped rather than fetched and installed again.
func (a *agent) rollback(ctx context.Context, head release.Hash, why error) (Outcome, error) {
	a.log.Error("reverted", "release", head.String(), "err", why)
	if err := a.inst.MarkFailed(head); err != nil {
		a.log.Error("failed", "err", err)
	}
	// No .old means the install is already undone — a caller whose Restart
	// is the exec handoff reverts before it reports failure — or there was
	// no previous binary to go back to. Either way there is nothing to
	// revert and nothing to restart onto.
	if _, err := os.Stat(a.inst.OldPath()); err != nil {
		return OutcomeRolledBack, why
	}
	if err := a.inst.Revert(); err != nil {
		return OutcomeRolledBack, errors.Join(why, err)
	}
	if a.hooks.Restart != nil {
		a.log.Info("restart", "reverted", true)
		if err := a.hooks.Restart(ctx); err != nil {
			return OutcomeRolledBack, errors.Join(why, fmt.Errorf("agent: restarting the previous release: %w", err))
		}
	}
	return OutcomeRolledBack, why
}

// sleep waits for d and reports whether it got there before ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
