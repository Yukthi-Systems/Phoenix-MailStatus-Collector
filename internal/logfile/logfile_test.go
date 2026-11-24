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

package logfile

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotateCompressPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.log")
	w, err := Open(Options{Path: path, MaxSize: 100, MaxBackups: 2, Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	line := strings.Repeat("x", 59) + "\n" // 60 bytes: one line per file
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, _ := filepath.Glob(filepath.Join(dir, "*"))
	want := []string{
		filepath.Join(dir, "collector-2026-10-26T12-00-03.000.log.gz"),
		filepath.Join(dir, "collector-2026-10-26T12-00-04.000.log.gz"),
		path,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	f, err := os.Open(want[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	if string(b) != line {
		t.Fatalf("backup content %q", b)
	}
}

func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "collector.log")
	for _, s := range []string{"a\n", "b\n"} {
		w, err := Open(Options{Path: path, MaxSize: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(s))
		w.Close()
	}
	b, _ := os.ReadFile(path)
	if string(b) != "a\nb\n" {
		t.Fatalf("content %q", b)
	}
}

func TestMaxAge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.log")
	old := filepath.Join(dir, "collector-2026-01-01T00-00-00.000.log.gz")
	other := filepath.Join(dir, "collector-notes.log") // not a backup: kept
	for _, f := range []string{old, other} {
		os.WriteFile(f, []byte("x"), 0o600)
		os.Chtimes(f, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	}
	w, err := Open(Options{Path: path, MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old backup not removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated file removed")
	}
}
