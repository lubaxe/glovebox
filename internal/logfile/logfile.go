// Package logfile keeps a copy of glovebox's log on the backingfiles
// partition. The journal is kept in memory to spare the SD card, so it's
// lost whenever the car cuts the Pi's power, which is exactly when the log
// is most needed.
package logfile

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
)

// File is a size-capped log file: when it passes Max bytes it's renamed to
// <path>.1, replacing the previous one. Each write is synced to the card, so
// a line written just before a power cut survives. It writes nothing until
// Open is called, so it can be set up before the partition is mounted.
type File struct {
	Max int64

	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// Open starts writing to path, appending to what's there.
func (l *File) Open(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open(path)
}

func (l *File) open(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_SYNC, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.path, l.f, l.size = path, f, fi.Size()
	return nil
}

// Write implements io.Writer. Errors are swallowed once the file is open:
// a full or failing card mustn't stop glovebox.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return len(p), nil
	}
	if l.Max > 0 && l.size+int64(len(p)) > l.Max {
		l.f.Close()
		l.f = nil
		_ = os.Rename(l.path, l.path+".1")
		if err := l.open(l.path); err != nil {
			return len(p), nil
		}
	}
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

// Close stops writing.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Fanout is a slog.Handler that passes each record to all its handlers.
type Fanout []slog.Handler

func (h Fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, x := range h {
		if x.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h Fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, x := range h {
		if x.Enabled(ctx, r.Level) {
			errs = append(errs, x.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (h Fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(Fanout, len(h))
	for i, x := range h {
		out[i] = x.WithAttrs(attrs)
	}
	return out
}

func (h Fanout) WithGroup(name string) slog.Handler {
	out := make(Fanout, len(h))
	for i, x := range h {
		out[i] = x.WithGroup(name)
	}
	return out
}
