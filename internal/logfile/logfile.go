// Copyright (C) 2026 Yukthi Systems Private Limited
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License version 3
// as published by the Free Software Foundation.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// version 3 along with this program. If not, see
// <https://www.gnu.org/licenses/>.

// Package logfile is an io.Writer that appends to a file and rotates it by
// size. Rotated files are renamed to <name>-<UTC time><ext>, gzipped in the
// background, and pruned by count and age.
package logfile

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures a File.
type Options struct {
	Path       string
	MaxSize    int64         // rotate before a write would exceed this many bytes; 0: never
	MaxBackups int           // rotated files kept; 0: unlimited
	MaxAge     time.Duration // rotated files older than this are removed; 0: no limit
	Compress   bool          // gzip rotated files
}

// File is safe for concurrent use.
type File struct {
	opt  Options
	mu   sync.Mutex
	f    *os.File
	size int64

	work chan struct{} // wakes the compress/prune worker
	done chan struct{} // closed when the worker exits
	now  func() time.Time
}

// timeFormat sorts lexically in time order and is valid in file names.
const timeFormat = "2006-01-02T15-04-05.000"

// Open opens (or creates) opt.Path for appending, creating its directory.
func Open(opt Options) (*File, error) {
	if opt.Path == "" {
		return nil, errors.New("logfile: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(opt.Path), 0o750); err != nil {
		return nil, err
	}
	w := &File{
		opt:  opt,
		work: make(chan struct{}, 1),
		done: make(chan struct{}),
		now:  time.Now,
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	go w.worker()
	w.kick() // finish anything left from a previous run
	return w, nil
}

// open opens the active log file for appending and records its size.
func (w *File) open() error {
	f, err := os.OpenFile(w.opt.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Write appends p, rotating first if p would push the file past MaxSize.
func (w *File) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.opt.MaxSize > 0 && w.size > 0 && w.size+int64(len(p)) > w.opt.MaxSize {
		if err := w.rotate(); err != nil {
			// Keep logging to the current file rather than losing the line.
			fmt.Fprintf(os.Stderr, "logfile: rotate %s: %v\n", w.opt.Path, err)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// Rotate starts a new file now.
func (w *File) Rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return os.ErrClosed
	}
	return w.rotate()
}

// rotate renames the active file to a timestamped backup and opens a new
// one. The caller holds w.mu.
func (w *File) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	renameErr := os.Rename(w.opt.Path, w.backupName(w.now()))
	if err := w.open(); err != nil {
		return errors.Join(renameErr, err)
	}
	if renameErr == nil {
		w.kick()
	}
	return renameErr
}

// Close closes the file and waits for pending compression to finish.
func (w *File) Close() error {
	w.mu.Lock()
	var err error
	if w.f != nil {
		err = w.f.Close()
		w.f = nil
		close(w.work)
	}
	w.mu.Unlock()
	<-w.done
	return err
}

// kick wakes the background worker without blocking.
func (w *File) kick() {
	select {
	case w.work <- struct{}{}:
	default:
	}
}

// worker compresses and prunes rotated files whenever it is kicked, until
// Close closes w.work.
func (w *File) worker() {
	defer close(w.done)
	for range w.work {
		if err := w.cleanup(); err != nil {
			fmt.Fprintf(os.Stderr, "logfile: %v\n", err)
		}
	}
}

// prefixExt splits "collector.log" into "collector-" and ".log".
func (w *File) prefixExt() (string, string) {
	ext := filepath.Ext(w.opt.Path)
	return strings.TrimSuffix(w.opt.Path, ext) + "-", ext
}

// backupName returns the rotated file name for time t.
func (w *File) backupName(t time.Time) string {
	prefix, ext := w.prefixExt()
	return prefix + t.UTC().Format(timeFormat) + ext
}

// backups lists rotated files, oldest first.
func (w *File) backups() ([]string, error) {
	prefix, ext := w.prefixExt()
	matches, err := filepath.Glob(globEscape(prefix) + "*" + globEscape(ext) + "*")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		stamp := strings.TrimPrefix(m, prefix)
		stamp = strings.TrimSuffix(strings.TrimSuffix(stamp, ".gz"), ext)
		if _, err := time.Parse(timeFormat, stamp); err == nil {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// cleanup gzips uncompressed backups (if enabled) and removes backups beyond
// MaxBackups or older than MaxAge.
func (w *File) cleanup() error {
	files, err := w.backups()
	if err != nil {
		return err
	}
	var errs []error
	if w.opt.Compress {
		for i, f := range files {
			if strings.HasSuffix(f, ".gz") {
				continue
			}
			if err := gzipFile(f); err != nil {
				errs = append(errs, err)
				continue
			}
			files[i] = f + ".gz"
		}
	}
	var cutoff time.Time
	if w.opt.MaxAge > 0 {
		cutoff = w.now().Add(-w.opt.MaxAge)
	}
	for i, f := range files {
		remove := w.opt.MaxBackups > 0 && i < len(files)-w.opt.MaxBackups
		if !remove && w.opt.MaxAge > 0 {
			if st, err := os.Stat(f); err == nil && st.ModTime().Before(cutoff) {
				remove = true
			}
		}
		if remove {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// gzipFile replaces path with path.gz, keeping its modification time.
func gzipFile(path string) (err error) {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := path + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(tmp)
		}
	}()
	zw := gzip.NewWriter(out)
	if _, err = io.Copy(zw, in); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
		return err
	}
	if err = os.Rename(tmp, path+".gz"); err != nil {
		return err
	}
	return os.Remove(path)
}

// globEscape quotes the filepath.Match metacharacters in s.
func globEscape(s string) string {
	r := strings.NewReplacer(`*`, `\*`, `?`, `\?`, `[`, `\[`, `\`, `\\`)
	return r.Replace(s)
}
