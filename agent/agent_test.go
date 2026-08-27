package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wjordan/go-binsync/release"
)

// TestOnceFollowsTheChain is the whole loop over a file:// store: two
// releases, a target on the first, one cycle.
func TestOnceFollowsTheChain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	f.publish(r1)
	f.install(r1)
	head := f.publish(r2)
	// If the chain is not taken, nothing else can produce r2.
	f.remove(release.BlobKey(head))

	h := &hooks{}
	o, err := Once(t.Context(), f.config(), h.Hooks())
	if err != nil || o != OutcomeUpdated {
		t.Fatalf("Once = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if o.ExitCode() != 0 {
		t.Errorf("exit code %d, want 0", o.ExitCode())
	}
	if !bytes.Equal(f.binary(), r2) {
		t.Error("the binary at the path is not the head release")
	}
	if got := strings.Join(h.seen(), ","); got != "restart,check" {
		t.Errorf("hooks fired %q, want restart,check", got)
	}
	inst := &release.Installer{Path: f.path}
	if !f.exists(inst.OldPath()) {
		t.Error("no .old: the previous release cannot be reverted to")
	}
	if f.exists(inst.PendingPath()) {
		t.Error(".pending survived a healthy update")
	}
	for _, want := range []string{"poll", "plan", "fetch", "apply", "install", "restart", "healthy"} {
		if !hasMsg(f.log, want) {
			t.Errorf("no %q line\n%s", want, f.log.dump())
		}
	}
	if !f.log.has("plan", "kind", "chain") {
		t.Errorf("plan was not a chain\n%s", f.log.dump())
	}
}

func hasMsg(s *sink, msg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		if l.msg == msg {
			return true
		}
	}
	return false
}

// TestChainOfTwoEdges: a target two releases behind applies both patches,
// oldest first (README guarantee 3, up to MaxChain behind).
func TestChainOfTwoEdges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2, r3 := relBytes(1), relBytes(2), relBytes(3)
	f.publish(r1)
	f.install(r1)
	f.publish(r2)
	head := f.publish(r3)
	// If either patch is skipped, nothing else can produce r3.
	f.remove(release.BlobKey(head))

	o, err := Once(t.Context(), f.config(), Hooks{})
	if err != nil || o != OutcomeUpdated {
		t.Fatalf("Once = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r3) {
		t.Fatal("the binary at the path is not the head release")
	}
	if got := f.log.count("apply"); got != 2 {
		t.Errorf("%d patches applied, want 2\n%s", got, f.log.dump())
	}
}

// TestOnceTakesTheBlobWhenTheFileDrifted is README guarantee 4: a file whose
// hash is on no chain edge is not patched, it is replaced.
func TestOnceTakesTheBlobWhenTheFileDrifted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.publish(relBytes(1))
	r2 := relBytes(2)
	f.publish(r2)
	f.install([]byte("something nobody published"))

	o, err := Once(t.Context(), f.config(), Hooks{})
	if err != nil || o != OutcomeUpdated {
		t.Fatalf("Once = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r2) {
		t.Error("the binary at the path is not the head release")
	}
	if !f.log.has("plan", "kind", "blob") {
		t.Errorf("plan was not the blob\n%s", f.log.dump())
	}
}

// TestOnceInstallsOntoNothing: a target with no binary yet is drift with no
// hash at all.
func TestOnceInstallsOntoNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)

	o, err := Once(t.Context(), f.config(), Hooks{})
	if err != nil || o != OutcomeUpdated {
		t.Fatalf("Once = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the binary at the path is not the head release")
	}
}

// TestOnceIsAtHead: the second cycle over an unchanged stream does nothing
// and touches nothing.
func TestOnceIsAtHead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.install(r1)

	h := &hooks{}
	o, err := Once(t.Context(), f.config(), h.Hooks())
	if err != nil || o != OutcomeAtHead {
		t.Fatalf("Once = %v, %v; want at-head\n%s", o, err, f.log.dump())
	}
	if len(h.seen()) != 0 {
		t.Errorf("hooks fired %v on a target that was already at head", h.seen())
	}
	if f.exists((&release.Installer{Path: f.path}).OldPath()) {
		t.Error("a no-op cycle wrote .old")
	}
}

// TestOnceRejectsACorruptPatch: a patch that does not hash to what the
// pointer says is exit code 3, and the binary is not touched. It does not
// fall back to the blob -- only a patch this build cannot *read* does that.
func TestOnceRejectsACorruptPatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	from := f.publish(r1)
	f.install(r1)
	to := f.publish(r2)

	key := release.PatchKey(from, to)
	f.put(key, bytes.Repeat([]byte{0xa5}, len(f.get(key))))

	o, err := Once(t.Context(), f.config(), Hooks{})
	if o != OutcomeVerifyFailed || !errors.Is(err, errVerify) {
		t.Fatalf("Once = %v, %v; want verify-failed\n%s", o, err, f.log.dump())
	}
	if o.ExitCode() != 3 {
		t.Errorf("exit code %d, want 3", o.ExitCode())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the binary changed under a rejected patch")
	}
	inst := &release.Installer{Path: f.path}
	if f.exists(inst.OldPath()) || f.exists(inst.PendingPath()) {
		t.Error("a rejected patch left install state behind")
	}
}

// TestOnceFallsBackToTheBlobOnAnUnreadablePatch is README guarantee 3: a
// transform this build does not implement costs a full download, not an
// error.
func TestOnceFallsBackToTheBlobOnAnUnreadablePatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	from := f.publish(r1)
	f.install(r1)
	to := f.publish(r2)

	// Byte 4 of the container is the transform number (docs/DESIGN.md 3.5).
	key := release.PatchKey(from, to)
	patch := f.get(key)
	patch[4] = 99
	f.put(key, patch)
	f.ptr.Chain[0].B3 = release.HashBytes(patch)
	f.putPointer()

	o, err := Once(t.Context(), f.config(), Hooks{})
	if err != nil || o != OutcomeUpdated {
		t.Fatalf("Once = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r2) {
		t.Error("the binary at the path is not the head release")
	}
	if !f.log.has("apply", "fallback", "blob") {
		t.Errorf("no fallback to the blob\n%s", f.log.dump())
	}
}

// TestFailedCheckRollsBackAndIsSkipped walks README guarantee 7 end to end:
// the health check fails, the previous binary comes back, the service is
// restarted onto it, the release is recorded, and the next cycles leave it
// alone until the pointer moves.
func TestFailedCheckRollsBackAndIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2, r3 := relBytes(1), relBytes(2), relBytes(3)
	f.publish(r1)
	f.install(r1)
	bad := f.publish(r2)

	h := &hooks{check: errors.New("connection refused")}
	a := f.agent(h.Hooks())
	o, err := a.cycle(t.Context())
	if o != OutcomeRolledBack || err == nil {
		t.Fatalf("cycle = %v, %v; want rolled-back\n%s", o, err, f.log.dump())
	}
	if o.ExitCode() != 5 {
		t.Errorf("exit code %d, want 5", o.ExitCode())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Fatal("the previous release is not back at the path")
	}
	inst := &release.Installer{Path: f.path}
	if f.exists(inst.PendingPath()) {
		t.Error(".pending survived the revert")
	}
	if got, ok := inst.Failed(); !ok || got != bad {
		t.Errorf("failed marker = %v, %v; want %s", got, ok, bad)
	}
	if !f.log.has("reverted", "release", bad.String()) {
		t.Errorf("no reverted line\n%s", f.log.dump())
	}
	// One restart onto the new release, the check polled until it gave up,
	// then one restart onto the reverted one.
	calls := h.seen()
	if len(calls) < 3 || calls[0] != "restart" || calls[len(calls)-1] != "restart" {
		t.Fatalf("hooks fired %v, want restart, checks, restart", calls)
	}
	for _, c := range calls[1 : len(calls)-1] {
		if c != "check" {
			t.Fatalf("hooks fired %v between the restarts", calls)
		}
	}

	// A fresh agent, as a restarted `go-binsync agent` would be: the marker,
	// not memory, is what stops the crash loop.
	h2 := &hooks{}
	o, err = f.agent(h2.Hooks()).cycle(t.Context())
	if o != OutcomeAtHead {
		t.Fatalf("second cycle = %v, %v; want at-head\n%s", o, err, f.log.dump())
	}
	if len(h2.seen()) != 0 {
		t.Errorf("the skipped release was restarted anyway: %v", h2.seen())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the skipped release was installed anyway")
	}

	// The pointer moves: the marker is stale and the target updates again.
	f.publish(r3)
	h3 := &hooks{}
	o, err = f.agent(h3.Hooks()).cycle(t.Context())
	if o != OutcomeUpdated || err != nil {
		t.Fatalf("third cycle = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r3) {
		t.Error("the target did not reach the new head")
	}
	if _, ok := inst.Failed(); ok {
		t.Error("the failed marker outlived the head it named")
	}
}

// TestFailedRestartRollsBack: a restart command that exits non-zero is the
// same outcome as a failed health check (docs/DESIGN.md 6.4).
func TestFailedRestartRollsBack(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	f.publish(r1)
	f.install(r1)
	bad := f.publish(r2)

	calls := 0
	a := f.agent(Hooks{Restart: func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("systemctl: unit failed")
		}
		return nil
	}})
	o, err := a.cycle(t.Context())
	if o != OutcomeRolledBack || err == nil {
		t.Fatalf("cycle = %v, %v; want rolled-back\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the previous release is not back at the path")
	}
	if calls != 2 {
		t.Errorf("restart ran %d times, want 2 (the new release, then the reverted one)", calls)
	}
	if got, ok := (&release.Installer{Path: f.path}).Failed(); !ok || got != bad {
		t.Errorf("failed marker = %v, %v; want %s", got, ok, bad)
	}
}

// TestRollbackLeavesTheHandoffAlone: a Restart that reverted before
// reporting failure -- the embedded exec handoff -- must not be called a
// second time to restart a binary it already put back.
func TestRollbackLeavesTheHandoffAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	f.publish(r1)
	f.install(r1)
	f.publish(r2)

	inst := &release.Installer{Path: f.path}
	calls := 0
	a := f.agent(Hooks{Restart: func(context.Context) error {
		calls++
		if err := inst.Revert(); err != nil {
			t.Error(err)
		}
		return errors.New("the new process did not report ready")
	}})
	if o, _ := a.cycle(t.Context()); o != OutcomeRolledBack {
		t.Fatalf("cycle = %v; want rolled-back\n%s", o, f.log.dump())
	}
	if calls != 1 {
		t.Errorf("restart ran %d times, want 1", calls)
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the previous release is not back at the path")
	}
}

// TestNoPathToHead is README exit code 4: nothing published reaches this
// target.
func TestNoPathToHead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.publish(relBytes(1))
	f.install([]byte("something nobody published"))
	f.ptr.Head.Blob = nil // the blob has not been uploaded and never will be
	f.ptr.Chain = nil
	f.putPointer()

	o, err := Once(t.Context(), f.config(), Hooks{})
	if o != OutcomeNoPath || err == nil {
		t.Fatalf("Once = %v, %v; want no-path\n%s", o, err, f.log.dump())
	}
	if o.ExitCode() != 4 {
		t.Errorf("exit code %d, want 4", o.ExitCode())
	}
}

// TestPollIsConditionalAndOrdered covers the two things a poll must do
// besides read: cost nothing when the pointer has not moved, and ignore one
// that went backwards (README guarantee 2).
func TestPollIsConditionalAndOrdered(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	f.publish(r1)
	f.install(r1)

	a := f.agent(Hooks{})
	if o, err := a.cycle(t.Context()); o != OutcomeAtHead || err != nil {
		t.Fatalf("first cycle = %v, %v", o, err)
	}
	if !f.log.has("poll", "changed", "true") {
		t.Errorf("the first poll did not read the pointer\n%s", f.log.dump())
	}
	if o, err := a.cycle(t.Context()); o != OutcomeAtHead || err != nil {
		t.Fatalf("second cycle = %v, %v", o, err)
	}
	if !f.log.has("poll", "changed", "false") {
		t.Errorf("the second poll was not conditional\n%s", f.log.dump())
	}

	// A pointer that goes backwards is a replay: it can hold the target
	// back, not move it.
	f.publish(r2)
	f.ptr.Seq = 0
	f.putPointer()
	if o, err := a.cycle(t.Context()); o != OutcomeAtHead || err != nil {
		t.Fatalf("third cycle = %v, %v", o, err)
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("a replayed pointer moved the target")
	}
	if !f.log.has("poll", "ignored", "the pointer is older than the last one") {
		t.Errorf("the replay was not reported\n%s", f.log.dump())
	}
}

// TestUnknownFormatKeepsTheBinary: store format bumps are forward-only
// (docs/DESIGN.md 4.2).
func TestUnknownFormatKeepsTheBinary(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.install(r1)
	f.publish(relBytes(2))
	f.ptr.Format = release.Format + 1
	f.putPointer()

	o, err := Once(t.Context(), f.config(), Hooks{})
	if o != OutcomeAtHead || err != nil {
		t.Fatalf("Once = %v, %v; want at-head\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("a pointer this build cannot read moved the target")
	}
}

// TestMaxSizeRefusesTheRelease: the pointer decides an allocation, so it is
// bounded (docs/DESIGN.md 8).
func TestMaxSizeRefusesTheRelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.publish(relBytes(1))

	cfg := f.config()
	cfg.MaxSize = 4096
	o, err := Once(t.Context(), cfg, Hooks{})
	if o != OutcomeError || err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Once = %v, %v; want an error about the limit", o, err)
	}
	if f.exists(f.path) {
		t.Error("an oversized release was installed anyway")
	}
}

// TestLoopStopsWithItsContext: Loop is the CLI's main; it must come back when
// the process is asked to stop, having done at least one cycle.
func TestLoopStopsWithItsContext(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)

	cfg := f.config()
	cfg.Poll = time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Loop(ctx, cfg, Hooks{}) }()

	deadline := time.After(2 * time.Second)
	for !f.exists(f.path) {
		select {
		case <-deadline:
			t.Fatalf("the loop never installed anything\n%s", f.log.dump())
		default:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Loop = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not return after its context was cancelled")
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the loop did not install the head release")
	}
}

// TestLoopRefusesAConfigItCannotStartWith: everything else is logged and
// retried, so a returned error means "this will never work".
func TestLoopRefusesAConfigItCannotStartWith(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no path", Config{Store: "file:///tmp"}},
		{"no scheme", Config{Store: "/tmp/releases", Path: "/tmp/server"}},
		{"unknown scheme", Config{Store: "carrier-pigeon://host/x", Path: "/tmp/server"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Loop(t.Context(), tc.cfg, Hooks{}); err == nil {
				t.Fatal("Loop accepted a config it cannot use")
			}
			if o, err := Once(t.Context(), tc.cfg, Hooks{}); err == nil || o != OutcomeError {
				t.Fatalf("Once = %v, %v; want an error", o, err)
			}
		})
	}
}

// TestPollBackoffAndDefaults pins the two schedules README 5 states.
func TestPollBackoffAndDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		store string
		want  time.Duration
	}{
		{"file:///srv/releases", time.Second},
		{"s3://bucket/prefix", 5 * time.Second},
		{"https://host/prefix", 5 * time.Second},
	} {
		if got := defaultPoll(tc.store); got != tc.want {
			t.Errorf("defaultPoll(%q) = %s, want %s", tc.store, got, tc.want)
		}
	}
}

func TestOutcomeExitCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		o    Outcome
		code int
		name string
	}{
		{OutcomeAtHead, 0, "at-head"},
		{OutcomeUpdated, 0, "updated"},
		{OutcomeError, 1, "error"},
		{OutcomeVerifyFailed, 3, "verify-failed"},
		{OutcomeNoPath, 4, "no-path"},
		{OutcomeRolledBack, 5, "rolled-back"},
	} {
		if got := tc.o.ExitCode(); got != tc.code {
			t.Errorf("%s.ExitCode() = %d, want %d", tc.name, got, tc.code)
		}
		if got := tc.o.String(); got != tc.name {
			t.Errorf("String() = %q, want %q", got, tc.name)
		}
	}
}

// TestCycleIsSerialised is README guarantee 8 from the loop's side: one
// update at a time, so an install never overlaps another.
func TestCycleReReadsAfterAFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	from := f.publish(r1)
	f.install(r1)
	to := f.publish(r2)

	key := release.PatchKey(from, to)
	good := f.get(key)
	f.put(key, bytes.Repeat([]byte{0xa5}, len(good)))

	a := f.agent(Hooks{})
	if o, _ := a.cycle(t.Context()); o != OutcomeVerifyFailed {
		t.Fatalf("first cycle = %v; want verify-failed", o)
	}
	// A cached ETag here would answer the retry with 304 and park the
	// target on r1 forever.
	f.put(key, good)
	if o, err := a.cycle(t.Context()); o != OutcomeUpdated || err != nil {
		t.Fatalf("second cycle = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r2) {
		t.Error("the retry did not reach the head release")
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
