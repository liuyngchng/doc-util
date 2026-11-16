package slogutil

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dailyWriter writes to a log file that rotates once per day (local timezone).
// On rotation the previous file is gzip-compressed as <base>.<date>.gz and a new
// file is opened for the current day. It is safe for concurrent use.
type dailyWriter struct {
	mu       sync.Mutex
	dir      string
	base     string // base file name, e.g. "server.log"
	f        *os.File
	openDate string // date of the currently open file, "2006-01-02"
}

// NewDailyWriter opens (or creates) the log file dir/base and returns a writer
// that rotates it daily, compressing the previous day's log on rotation.
func NewDailyWriter(dir, base string) (io.WriteCloser, error) {
	w := &dailyWriter{dir: dir, base: base}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.rotate(); err != nil {
		return 0, err
	}
	return w.f.Write(p)
}

func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotate rolls the log file over when the day has changed. It must be called
// with w.mu held.
func (w *dailyWriter) rotate() error {
	today := time.Now().Format("2006-01-02")
	if w.f != nil && w.openDate == today {
		return nil
	}

	path := filepath.Join(w.dir, w.base)

	// Compress the previous day's file (best-effort, non-fatal).
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: closing log file %s: %v\n", path, err)
		}
		w.f = nil
		if err := compressFile(path, filepath.Join(w.dir, w.base+"."+w.openDate+".gz")); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: compressing log file %s: %v\n", path, err)
		}
	} else if fi, err := os.Stat(path); err == nil {
		// No file open yet (first open after startup). If an existing log
		// file is from a previous day, compress it before starting today's.
		fileDate := fi.ModTime().Format("2006-01-02")
		if fileDate != today {
			if err := compressFile(path, filepath.Join(w.dir, w.base+"."+fileDate+".gz")); err != nil {
				fmt.Fprintf(os.Stderr, "WARN: compressing log file %s: %v\n", path, err)
			}
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.f = f
	w.openDate = today
	return nil
}

// compressFile gzip-compresses src into dst and removes src. The .gz file is
// removed first so re-compressing (e.g. after a restart within the same day)
// does not corrupt an existing archive.
func compressFile(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return nil // nothing to compress
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}

	return os.Remove(src)
}
