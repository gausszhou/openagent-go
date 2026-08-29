package server

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yusheng-g/openagent-go/cmd/cli/config"
	"github.com/yusheng-g/openagent-go/kernel"
)

// logWriter holds the active log writer so reconfigureLogLevel can
// rebuild the slog handler with the same writer but a new level.
// SetupLog stores the writer here; reconfigureLogLevel reads it.
var logWriter atomic.Pointer[io.Writer]

// logFilePath holds the resolved log file path (empty when logging is
// discarded). SetupLog stores it; the settings tool reads it so the agent
// sees the concrete path the server is writing to, not a re-derivation.
var logFilePath atomic.Pointer[string]

// SetupLog configures slog + log package output. Writes to a rotated log
// file when cfg.File is set; otherwise discards log output silently.
//
// IMPORTANT: does NOT write to os.Stderr. The ACP protocol uses stderr as
// a control pipe — any log output there fills the pipe buffer and blocks
// the process. Use fmt.Fprintf(os.Stderr, ...) for intentional console output.
func SetupLog(cfg config.LogConfig) (func(), error) {
	level := parseLevel(cfg.Level)

	mw := &multiCloser{}

	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0755); err != nil {
			return nil, fmt.Errorf("log dir: %w", err)
		}
		rw, err := newRotWriter(cfg.File, cfg.MaxSize, cfg.MaxBackups)
		if err != nil {
			return nil, fmt.Errorf("log file: %w", err)
		}
		mw.AddCloser(rw)
	}

	// When no log file is configured, fall back to discarding.
	if len(mw.writers) == 0 {
		mw.AddWriter(io.Discard)
	}

	// Store the writer and level for reconfigureLogLevel.
	var w io.Writer = mw
	logWriter.Store(&w)
	logFilePath.Store(&cfg.File)

	// Use a level-switchable handler so reconfigureLogLevel can change
	// the level without rebuilding the handler (and losing the writer).
	h := slog.NewJSONHandler(mw, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(h))

	// Redirect log.Printf etc. to the same writer.
	log.SetOutput(mw)

	return func() { mw.Close() }, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return kernel.LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// reconfigureLogLevel changes the global slog level at runtime without
// losing the log writer. Rebuilds the handler with the stored writer
// (from SetupLog) and the new level. Called by the settings watcher
// when log.level changes in settings.json.
func reconfigureLogLevel(level string) {
	lvl := parseLevel(level)
	w := logWriter.Load()
	if w == nil {
		// SetupLog not called yet (shouldn't happen in normal flow).
		slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{
			Level: lvl,
		})))
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(*w, &slog.HandlerOptions{
		Level: lvl,
	})))
}

// ── multiCloser ──

type multiCloser struct {
	mu      sync.Mutex
	writers []io.Writer
	closers []io.Closer
}

func (m *multiCloser) AddWriter(w io.Writer) {
	m.writers = append(m.writers, w)
	if c, ok := w.(io.Closer); ok {
		m.closers = append(m.closers, c)
	}
}

func (m *multiCloser) AddCloser(c io.Closer) {
	m.writers = append(m.writers, c.(io.Writer))
	m.closers = append(m.closers, c)
}

func (m *multiCloser) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lastN int
	var lastErr error
	for _, w := range m.writers {
		n, err := w.Write(p)
		if err == nil {
			lastN = n
		} else {
			lastErr = err
		}
	}
	return lastN, lastErr
}

func (m *multiCloser) Close() {
	for _, c := range m.closers {
		_ = c.Close()
	}
}

// ── rotWriter ──

type rotWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int
	maxBackups int
	f          *os.File
	size       int64
}

func newRotWriter(path string, maxSizeMB, maxBackups int) (*rotWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	fi, _ := f.Stat()
	return &rotWriter{
		path:       path,
		maxSize:    maxSizeMB * 1024 * 1024,
		maxBackups: maxBackups,
		f:          f,
		size:       fi.Size(),
	}, nil
}

func (w *rotWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.f.Write(p)
	w.size += int64(n)

	if w.size >= int64(w.maxSize) {
		w.rotate()
	}
	return n, err
}

func (w *rotWriter) rotate() {
	w.f.Close()

	ts := fmt.Sprintf("%d", time.Now().Unix())
	backup := w.path + "." + ts
	os.Rename(w.path, backup)

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	w.f = f
	w.size = 0

	dir := filepath.Dir(w.path)
	base := filepath.Base(w.path)
	entries, _ := os.ReadDir(dir)
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base+".") {
			if e.Name() == base+"."+ts {
				continue
			}
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(backups)
	if w.maxBackups > 0 && len(backups) > w.maxBackups {
		for _, p := range backups[:len(backups)-w.maxBackups] {
			os.Remove(p)
		}
	}
}

func (w *rotWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
