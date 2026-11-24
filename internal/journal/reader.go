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

package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Reader follows the journal by running `journalctl -f -o json`.
//
// journalctl is used instead of sd-journal bindings so the binary needs no cgo
// and no libsystemd at build time; journalctl handles rotation, vacuuming and
// file-format changes for us.
type Reader struct {
	Journalctl  string   // path or name of journalctl
	Identifiers []string // SYSLOG_IDENTIFIER values to follow (OR'ed)
	Log         *slog.Logger
}

// Follow streams entries after pos to out until ctx is cancelled or
// journalctl exits. It always returns a non-nil error.
func (r *Reader) Follow(ctx context.Context, pos Position, out chan<- Entry) error {
	err := r.run(ctx, r.args(pos, true), out)
	if errors.Is(err, errCursor) && !pos.Time.IsZero() {
		// The cursor points at entries that were vacuumed. Resume by time; the
		// overlap is harmless because event IDs are deterministic.
		r.Log.Warn("saved journal cursor is no longer valid, resuming by timestamp",
			"since", pos.Time, "cursor", pos.Cursor)
		return r.run(ctx, r.args(Position{Time: pos.Time}, true), out)
	}
	return err
}

// Stream reads `journalctl -o json` output (e.g. a saved export) from in.
func (r *Reader) Stream(ctx context.Context, in io.Reader, out chan<- Entry) error {
	return r.scan(ctx, in, out)
}

// errCursor is returned by run when journalctl did not accept --after-cursor.
var errCursor = errors.New("journalctl rejected the cursor")

// args builds the journalctl command line that resumes at pos: after the
// cursor, else since its time, else (first start) from new entries only.
func (r *Reader) args(pos Position, follow bool) []string {
	args := []string{"--output=json", "--no-pager", "--quiet"}
	if follow {
		args = append(args, "--follow")
	}
	for _, id := range r.Identifiers {
		args = append(args, "--identifier="+id)
	}
	switch {
	case pos.Cursor != "":
		args = append(args, "--after-cursor="+pos.Cursor)
	case !pos.Time.IsZero():
		args = append(args, "--since=@"+strconv.FormatInt(pos.Time.Unix(), 10))
	default:
		args = append(args, "--lines=0") // first start: only new entries
	}
	return args
}

// run starts journalctl with args and streams its output to out until it
// exits or ctx is cancelled.
func (r *Reader) run(ctx context.Context, args []string, out chan<- Entry) error {
	cmd := exec.CommandContext(ctx, r.Journalctl, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr

	r.Log.Info("starting journalctl", "args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", r.Journalctl, err)
	}

	scanErr := r.scan(ctx, stdout, out)
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if msg := stderr.String(); strings.Contains(strings.ToLower(msg), "cursor") {
		return fmt.Errorf("%w: %s", errCursor, msg)
	}
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("journalctl exited: %w: %s", waitErr, stderr.String())
	}
	return errors.New("journalctl exited unexpectedly")
}

// scan decodes newline-delimited journal JSON from in and sends each entry to
// out. Undecodable lines are logged and skipped. It returns nil at EOF.
func (r *Reader) scan(ctx context.Context, in io.Reader, out chan<- Entry) error {
	br := bufio.NewReaderSize(in, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 1 {
			e, derr := Decode(line)
			if derr != nil {
				// One undecodable entry must not stop the pipeline.
				r.Log.Warn("skipping journal entry", "error", derr)
			} else {
				select {
				case out <- e:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read journal: %w", err)
		}
	}
}

// tailBuffer keeps the last 4 KiB written to it (journalctl stderr).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p, keeping only the last 4 KiB. It never fails.
func (b *tailBuffer) Write(p []byte) (int, error) {
	const limit = 4096
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > limit {
		b.buf = b.buf[len(b.buf)-limit:]
	}
	return len(p), nil
}

// String returns the buffered text without surrounding white space.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
