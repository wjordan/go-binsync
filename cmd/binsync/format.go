package main

import (
	"io"
	"log/slog"
	"strconv"
	"time"
)

// newLogger is the CLI's output: slog text on stderr, without the timestamp
// that everything reading it -- a terminal, journald, a CI log -- stamps for
// itself.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

var byteUnits = [...]string{"B", "KB", "MB", "GB", "TB", "PB"}

// hbytes renders a byte count the way the documentation quotes them --
// "112 KB", "2.1 MB" -- in SI units, because that is what the sizes a user
// compares this against (a download, a bucket bill) are quoted in.
func hbytes(n int64) string {
	v := float64(n)
	i := 0
	for i < len(byteUnits)-1 && (v >= 999.95 || v <= -999.95) {
		v /= 1000
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	prec := 0
	if -10 < v && v < 10 {
		prec = 1
	}
	return strconv.FormatFloat(v, 'f', prec, 64) + " " + byteUnits[i]
}

// hdur rounds a duration to the precision a person cares about: "2.1s",
// "340ms". Go's own formatting would print 2.100000001s.
func hdur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}
