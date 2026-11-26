// Package logging builds the zerolog logger used by the server, the runner
// and the admin tool.
//
// Two formats:
//
//	pretty  human-readable console lines (colored when writing to a terminal)
//	json    one JSON object per line, for journald, Docker, Loki, …
package logging

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
)

const (
	FormatPretty = "pretty"
	FormatJSON   = "json"
)

type Options struct {
	Format string // pretty (default) | json
	Level  string // debug | info (default) | warn | error
	Out    io.Writer
}

// Validate reports unsupported settings (empty values mean defaults).
func (o Options) Validate() error {
	switch strings.ToLower(o.Format) {
	case "", FormatPretty, FormatJSON:
	default:
		return fmt.Errorf("log format %q: use pretty or json", o.Format)
	}
	if _, err := parseLevel(o.Level); err != nil {
		return err
	}
	return nil
}

func parseLevel(s string) (zerolog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return zerolog.InfoLevel, nil
	case "debug":
		return zerolog.DebugLevel, nil
	case "warn", "warning":
		return zerolog.WarnLevel, nil
	case "error":
		return zerolog.ErrorLevel, nil
	}
	return zerolog.InfoLevel, fmt.Errorf("log level %q: use debug, info, warn or error", s)
}

// New returns a logger for o. Invalid values fall back to the defaults; call
// Validate first to report them.
func New(o Options) zerolog.Logger {
	out := o.Out
	if out == nil {
		out = os.Stderr
	}
	lvl, _ := parseLevel(o.Level)
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.DurationFieldUnit = time.Millisecond
	zerolog.DurationFieldInteger = false
	if strings.ToLower(o.Format) == FormatJSON {
		return zerolog.New(out).Level(lvl).With().Timestamp().Logger()
	}
	cw := zerolog.ConsoleWriter{Out: out, TimeFormat: "2006-01-02 15:04:05", NoColor: !isTerminal(out)}
	return zerolog.New(cw).Level(lvl).With().Timestamp().Logger()
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}
