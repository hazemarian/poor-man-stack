// Package logger wires zerolog into pmcluster with two outputs:
//
//   - a colorized console writer (human-friendly) on stderr
//   - a JSON file writer at <data_dir>/logs/pmcluster-YYYY-MM-DD.log
//
// The file writer is the audit trail; the console writer is what the
// operator sees when they run `pmcluster cluster up` or attach to
// `pmcluster serve` interactively. CLI commands that produce primary
// user-visible output (passwords, tokens, tables) keep using fmt.Fprint
// to stdout — this logger is for status/progress/error lines, not
// the headline content the operator came for.
//
// Files are rotated by date (one per UTC day) and swept on serve startup
// (see Sweep) to drop anything older than 14 days.
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// RetentionDays is how long pmcluster keeps log files before sweeping.
// 14 days is enough to investigate a multi-day incident; longer than that
// and the operator should be shipping logs to OpenObserve anyway.
const RetentionDays = 14

// Options configures a logger. All fields are optional.
type Options struct {
	// LogsDir is the directory holding pmcluster-YYYY-MM-DD.log files.
	// If empty, file logging is disabled.
	LogsDir string

	// Level is "debug" / "info" / "warn" / "error". Default "info".
	Level string

	// Console controls whether to also emit human-readable lines.
	// True for interactive commands (cluster up, serve), false for unit tests.
	Console bool

	// ConsoleOut is the writer for the console output. Defaults to os.Stdout
	// (so `pmcluster serve` log lines land in stdout, the same place an
	// operator's tail/grep is pointed at; tests can override).
	ConsoleOut io.Writer

	// Now is a clock injection seam — defaults to time.Now.
	Now func() time.Time
}

// dailyFileWriter is an io.Writer that re-opens its file when the UTC date
// rolls over. Cheap: each Write checks the date and only swaps when the day
// changes, so steady-state has one fstat-equivalent per write.
type dailyFileWriter struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	f       *os.File
	dateKey string
}

func newDailyFileWriter(dir string, now func() time.Time) (*dailyFileWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logger: mkdir %s: %w", dir, err)
	}
	w := &dailyFileWriter{dir: dir, now: now}
	if err := w.rotate(now()); err != nil {
		return nil, err
	}
	return w, nil
}

// rotate must be called with mu held OR before the first Write (in newDailyFileWriter).
func (w *dailyFileWriter) rotate(t time.Time) error {
	key := t.UTC().Format("2006-01-02")
	if w.f != nil && w.dateKey == key {
		return nil
	}
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	path := filepath.Join(w.dir, "pmcluster-"+key+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logger: open %s: %w", path, err)
	}
	w.f = f
	w.dateKey = key
	return nil
}

func (w *dailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotate(w.now()); err != nil {
		return 0, err
	}
	return w.f.Write(p)
}

// Close releases the underlying file handle. Safe to call from a
// signal-driven shutdown; further Writes will reopen lazily.
func (w *dailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// New constructs a zerolog.Logger from Options. Returns the logger plus a
// closer the caller should defer (no-op when file logging is disabled).
func New(opts Options) (zerolog.Logger, io.Closer, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	level := parseLevel(opts.Level)
	zerolog.TimeFieldFormat = time.RFC3339Nano

	var writers []io.Writer
	var closer io.Closer = noopCloser{}

	if opts.Console {
		out := opts.ConsoleOut
		if out == nil {
			out = os.Stdout
		}
		// ConsoleWriter formats lines like:
		//   2026-05-10T22:14:08+02:00 INF preflight passed
		console := zerolog.ConsoleWriter{
			Out:        out,
			TimeFormat: time.RFC3339,
		}
		writers = append(writers, console)
	}

	if opts.LogsDir != "" {
		fw, err := newDailyFileWriter(opts.LogsDir, opts.Now)
		if err != nil {
			return zerolog.Nop(), noopCloser{}, err
		}
		writers = append(writers, fw)
		closer = fw
	}

	if len(writers) == 0 {
		// Pure tests / nothing requested — return a Nop so callers don't
		// have to nil-check the logger.
		return zerolog.Nop(), noopCloser{}, nil
	}

	out := io.MultiWriter(writers...)
	logger := zerolog.New(out).Level(level).With().Timestamp().Logger()
	return logger, closer, nil
}

// Sweep deletes log files in dir older than RetentionDays. Best-effort:
// individual unlink failures are returned as a single joined error, but
// processing continues so one bad file doesn't block the rest.
//
// Called from serve startup; safe to call concurrently with active logging
// (the dailyFileWriter holds an open fd to today's file, which Linux/macOS
// keep usable even after unlink).
func Sweep(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sweep: read %s: %w", dir, err)
	}
	// Truncate to midnight UTC so a file dated exactly RetentionDays ago
	// is kept (we're comparing date stamps, not wall clocks).
	cutoff := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -RetentionDays)
	var problems []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Expected shape: pmcluster-YYYY-MM-DD.log
		if !strings.HasPrefix(name, "pmcluster-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, "pmcluster-"), ".log")
		t, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				problems = append(problems, name+": "+err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("sweep: %s", strings.Join(problems, "; "))
	}
	return nil
}

func parseLevel(s string) zerolog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return zerolog.DebugLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
