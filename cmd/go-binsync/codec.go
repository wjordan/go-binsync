package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/release"
)

// diff and patch are offline access to the codec, for development and for the
// benchmark harness (README.md 5).
func diff(log *slog.Logger, args []string) error {
	fs := newFlags("diff", "<old> <new> -o <patch>")
	out := fs.String("o", "", "write the patch here (required)")
	verbose := fs.Bool("v", false, "report where the patch bytes went")
	plain := fs.Bool("plain", false, "skip the Go-aware codec")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *out == "" {
		return exitf(codeUsage, "diff needs two binaries and -o")
	}
	old, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	next, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}

	start := time.Now()
	var st delta.Stats
	patch, err := delta.Encode(old, next, delta.Options{PlainOnly: *plain, Stats: &st})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, patch, 0o644); err != nil {
		return err
	}
	log.Info("go-binsync: encoded", "patch", hbytes(int64(len(patch))), "of", hbytes(int64(len(next))),
		"transform", st.Transform, "took", hdur(time.Since(start)))
	if *verbose {
		log.Info("go-binsync: patch bytes", "header", hbytes(int64(st.Header)), "layout", hbytes(int64(st.Layout)),
			"stage1a", hbytes(int64(st.Stage1a)), "stage1b", hbytes(int64(st.Stage1b)), "stage2", hbytes(int64(st.Stage2)))
		log.Info("go-binsync: functions", "total", st.Funcs, "matched", st.Matched, "new", st.NewFuncs,
			"mispredicted", hbytes(int64(st.PredictErr)))
		for _, n := range st.Notes {
			log.Info("go-binsync: " + n)
		}
	}
	return nil
}

func patchCmd(log *slog.Logger, args []string) error {
	fs := newFlags("patch", "<old> <patch> -o <new>")
	out := fs.String("o", "", "write the result here (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 || *out == "" {
		return exitf(codeUsage, "patch needs a binary, a patch and -o")
	}
	old, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	p, err := os.ReadFile(fs.Arg(1))
	if err != nil {
		return err
	}

	start := time.Now()
	var buf bytes.Buffer
	buf.Grow(len(old))
	if err := delta.Apply(old, p, &buf); err != nil {
		var ut *delta.ErrUnsupportedTransform
		if errors.As(err, &ut) {
			return err
		}
		// Everything else Apply refuses is the result not being what the
		// patch promised (README.md 5, exit code 3).
		return &exitError{codeVerify, err}
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o755); err != nil {
		return err
	}
	log.Info("go-binsync: applied", "release", release.HashBytes(buf.Bytes()),
		"size", hbytes(int64(buf.Len())), "took", hdur(time.Since(start)))
	return nil
}
