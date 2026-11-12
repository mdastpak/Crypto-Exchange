// Package logging makes every log line of a process structured (#50): a
// log/slog record, as JSON or text, at the level of
// observability.logging.level, with secrets, sign-in codes, tokens, and
// contact details redacted before they are written. The lines of the
// standard log package, which most of the code writes, become records too,
// with a level that their prefix gives.
package logging

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// Options are what a logger is built from.
type Options struct {
	// Level is debug, info, warn, or error; info when empty.
	Level string
	// Format is json or text; json when empty.
	Format string
	// Output receives the records; os.Stdout when nil.
	Output io.Writer
}

// OptionsOf returns the options of observability.logging in cfg. A file
// output opens the file, which the caller closes with the returned closer.
func OptionsOf(cfg *config.Configuration) (Options, io.Closer, error) {
	l := cfg.Observability.Logging
	opts := Options{Level: l.Level, Format: l.Format}
	switch strings.ToLower(l.Output) {
	case "", "stdout":
		opts.Output = os.Stdout
	case "stderr":
		opts.Output = os.Stderr
	case "file":
		if l.FilePath == "" {
			return opts, nil, fmt.Errorf("observability.logging.output is file, but file_path is empty")
		}
		f, err := os.OpenFile(l.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return opts, nil, fmt.Errorf("open the log file: %w", err)
		}
		opts.Output = f
		return opts, f, nil
	default:
		return opts, nil, fmt.Errorf("observability.logging.output must be stdout, stderr, or file, not %q", l.Output)
	}
	return opts, nil, nil
}

// ParseLevel returns the slog level of a name.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("observability.logging.level must be debug, info, warn, or error, not %q", name)
}

// New returns a logger of opts, which redacts what it writes.
func New(opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	ho := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}
	var h slog.Handler
	switch strings.ToLower(opts.Format) {
	case "", "json":
		h = slog.NewJSONHandler(out, ho)
	case "text":
		h = slog.NewTextHandler(out, ho)
	default:
		return nil, fmt.Errorf("observability.logging.format must be json or text, not %q", opts.Format)
	}
	return slog.New(&redactingHandler{next: h}), nil
}

// Install makes logger the default of log/slog, and turns every line of the
// standard log package into a record of it. It returns a function that
// restores the previous ones, for tests.
func Install(logger *slog.Logger) (restore func()) {
	previous, flags, output := slog.Default(), log.Flags(), log.Writer()
	slog.SetDefault(logger)
	// slog.SetDefault sends the standard log package to the new handler
	// already, but at INFO for every line; the bridge gives each line the
	// level of its prefix.
	log.SetFlags(0)
	log.SetOutput(&bridge{logger: logger})
	return func() {
		slog.SetDefault(previous)
		log.SetFlags(flags)
		log.SetOutput(output)
	}
}

// bridge writes each line of the standard log package as a record.
type bridge struct {
	logger *slog.Logger
}

func (b *bridge) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	level, attrs := levelOf(msg)
	b.logger.LogAttrs(context.Background(), level, msg, attrs...)
	return len(p), nil
}

// levelOf returns the level of a line of the standard log package from its
// prefix: the conventions of this code base, which marks warnings and
// security events with "WARNING:" and "SECURITY:", and failures with words
// that start the line.
func levelOf(msg string) (slog.Level, []slog.Attr) {
	switch {
	case strings.HasPrefix(msg, "SECURITY:"):
		return slog.LevelWarn, []slog.Attr{slog.Bool("security", true)}
	case strings.HasPrefix(msg, "WARNING:"):
		return slog.LevelWarn, nil
	case strings.HasPrefix(msg, "ERROR:"), strings.HasPrefix(msg, "Failed"), strings.HasPrefix(msg, "Error"):
		return slog.LevelError, nil
	}
	return slog.LevelInfo, nil
}

// Setup installs the logger of observability.logging in cfg as the logger
// of the process (Install). The returned function closes a log file.
func Setup(cfg *config.Configuration) (closeFile func(), err error) {
	opts, file, err := OptionsOf(cfg)
	if err != nil {
		return nil, err
	}
	logger, err := New(opts)
	if err != nil {
		if file != nil {
			_ = file.Close()
		}
		return nil, err
	}
	Install(logger)
	return func() {
		if file != nil {
			_ = file.Close()
		}
	}, nil
}
