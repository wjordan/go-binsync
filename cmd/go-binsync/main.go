// Command go-binsync publishes a Go binary release to a store and keeps a
// deployed copy of it up to date. README.md 5 specifies its behaviour.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	// s3:// is a heavy dependency and registers itself; the CLI documents
	// the scheme, so the CLI is what imports it (docs/DESIGN.md 7).
	_ "github.com/wjordan/go-binsync/store/s3"
)

// Exit codes (README.md 5).
const (
	codeOK         = 0
	codeError      = 1
	codeUsage      = 2
	codeVerify     = 3
	codeNoPath     = 4
	codeRolledBack = 5
)

// exitError carries the exit code an error must produce. Anything else is a
// plain failure and exits 1.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitf(code int, format string, a ...any) error {
	return &exitError{code, fmt.Errorf(format, a...)}
}

func main() {
	log := newLogger(os.Stderr)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, log, os.Args[1:])
	if err == nil {
		return
	}
	code := codeError
	var e *exitError
	if errors.As(err, &e) {
		code = e.code
	}
	if code != codeUsage {
		log.Error(err.Error())
	} else {
		fmt.Fprintf(os.Stderr, "go-binsync: %s\n\n", err)
		usage(os.Stderr)
	}
	os.Exit(code)
}

func run(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return &exitError{codeUsage, errors.New("no command")}
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "publish":
		return publish(ctx, log, rest)
	case "agent":
		return agentCmd(ctx, log, rest)
	case "diff":
		return diff(log, rest)
	case "patch":
		return patchCmd(log, rest)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		return exitf(codeUsage, "unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `go-binsync — small, fast, verified updates of a deployed Go binary

  go-binsync publish <binary> <store>         publish a release
  go-binsync agent <store> <path>             keep a target's binary at the store's head
  go-binsync diff <old> <new> -o <patch>      encode a patch
  go-binsync patch <old> <patch> -o <new>     apply one, verifying the result

Stores: s3://bucket/prefix  https://host/prefix  file:///dir  ssh://host/dir
Run "go-binsync <command> -h" for one command's flags.
`)
}

// newFlags gives every subcommand the same shape: its own flag set, and
// stdlib flag's ExitOnError, whose exit code 2 is already the one README.md 5
// reserves for usage.
func newFlags(name, args string) *flag.FlagSet {
	fs := flag.NewFlagSet("go-binsync "+name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: go-binsync %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}
