package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"binsync/agent"
	"binsync/release"
)

// restartTimeout is README.md 5's bound on --restart. The loop imposes none,
// because only the caller knows what its restart costs; for a shell command
// the answer is a minute.
const restartTimeout = 60 * time.Second

func agentCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := newFlags("agent", "--restart CMD [--healthy URL|CMD] [--once] [--poll D] <store> <path>")
	restart := fs.String("restart", "", "shell command that restarts the service (required)")
	healthy := fs.String("healthy", "", "URL that must answer 2xx, or a shell command that must exit 0, after --restart")
	once := fs.Bool("once", false, "run one update cycle and exit")
	poll := fs.Duration("poll", 0, "pointer poll interval (default 5s, 1s for file://)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return exitf(codeUsage, "agent needs a store URL and a path")
	}
	if *restart == "" {
		return exitf(codeUsage, "agent needs --restart CMD")
	}

	h := newHooks(log, fs.Arg(1), *restart, *healthy)
	// Before the first poll: this agent may have died between installing a
	// release and seeing it healthy (docs/DESIGN.md 6.5).
	if err := h.recover(ctx); err != nil {
		log.Error("binsync: start-up check", "err", err)
	}
	cfg := agent.Config{Store: fs.Arg(0), Path: h.path, Poll: *poll, Logger: log}
	if !*once {
		return agent.Loop(ctx, cfg, h.hooks())
	}
	outcome, err := agent.Once(ctx, cfg, h.hooks())
	if code := outcome.ExitCode(); code != codeOK {
		if err == nil {
			err = fmt.Errorf("agent: %s", outcome)
		}
		return &exitError{code, err}
	}
	return nil
}

// hooks is the external target's half of an update: the two user commands
// that make an installed release take effect (docs/DESIGN.md 6.4).
type hooks struct {
	log     *slog.Logger
	inst    *release.Installer
	path    string
	restart string
	healthy string
	timeout time.Duration
}

func newHooks(log *slog.Logger, path, restart, healthy string) *hooks {
	return &hooks{
		log: log, inst: &release.Installer{Path: path},
		path: path, restart: restart, healthy: healthy, timeout: restartTimeout,
	}
}

// hooks builds what the loop calls. Check is nil without --healthy, which is
// what makes "--restart exited 0" the whole success contract.
func (h *hooks) hooks() agent.Hooks {
	a := agent.Hooks{Restart: h.runRestart}
	if h.healthy != "" {
		a.Check = h.check
	}
	return a
}

func (h *hooks) runRestart(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", h.restart).CombinedOutput()
	if err != nil {
		return fmt.Errorf("--restart %q: %w%s", h.restart, err, tail(out))
	}
	return nil
}

// check is one probe of --healthy; the loop decides how often to repeat it.
func (h *hooks) check(ctx context.Context) error {
	if !strings.HasPrefix(h.healthy, "http://") && !strings.HasPrefix(h.healthy, "https://") {
		out, err := exec.CommandContext(ctx, "sh", "-c", h.healthy).CombinedOutput()
		if err != nil {
			return fmt.Errorf("--healthy %q: %w%s", h.healthy, err, tail(out))
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.healthy, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("--healthy %s answered %s", h.healthy, resp.Status)
	}
	return nil
}

// recover is docs/DESIGN.md 6.5 for the external shape: a pending marker on
// start-up means the previous run died between installing a release and
// seeing it healthy, so the service is running -- or crash-looping on -- a
// release nobody confirmed.
func (h *hooks) recover(ctx context.Context) error {
	rel, pending, err := h.inst.Pending()
	if !pending {
		return err
	}
	if _, serr := os.Stat(h.inst.OldPath()); serr != nil {
		// Nothing to go back to, so the marker cannot be acted on; drop it
		// rather than test it again on every start.
		return errors.Join(err, h.inst.ClearPending())
	}
	h.log.Warn("binsync: the installed release was never confirmed healthy; reverting", "release", rel)
	// Record before reverting: a crash between the two must leave the release
	// skipped, not fetched and installed again.
	if merr := h.inst.MarkFailed(rel); merr != nil {
		h.log.Error("binsync: recording the failed release", "err", merr)
	}
	if rerr := h.inst.Revert(); rerr != nil {
		return errors.Join(err, rerr)
	}
	return errors.Join(err, h.runRestart(ctx))
}

// tail renders a failed command's output on the one line that reports it.
func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(s, "\n", "; ")
}
