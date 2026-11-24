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

package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/postfix"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// flakySink fails the first `fail` commits, then records batches.
type flakySink struct {
	fail    int
	calls   int
	batches []sink.Batch
}

func (s *flakySink) Commit(_ context.Context, b *sink.Batch) (int64, error) {
	s.calls++
	if s.calls <= s.fail {
		return 0, errors.New("broker down")
	}
	cp := *b
	cp.Events = append(cp.Events[:0:0], b.Events...)
	s.batches = append(s.batches, cp)
	return int64(len(b.Events)), nil
}

func newCollector(out sink.Sink, batch int) *Collector {
	return New(Options{
		BatchSize:     batch,
		FlushInterval: time.Hour, // only size and end of input trigger commits
		ExpireEvery:   time.Hour,
		Live:          true,
		ShutdownGrace: 5 * time.Second,
	}, postfix.NewParser("X-Ref-ID"),
		tracker.New(tracker.Config{SuccessCode: "255"}, quiet), out, quiet)
}

func entries(lines ...string) <-chan journal.Entry {
	ch := make(chan journal.Entry, len(lines))
	t0 := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	for i, l := range lines {
		ch <- journal.Entry{Cursor: "c" + strconv.Itoa(i), Time: t0.Add(time.Duration(i) * time.Second), Message: l}
	}
	close(ch)
	return ch
}

var mail = []string{
	"AAA111: warning: header X-Ref-ID: REF-1 from local; from=<app@x.in> to=<a@corp.com>",
	"AAA111: from=<app@x.in>, size=1, nrcpt=2 (queue active)",
	"AAA111: to=<a@corp.com>, relay=mx[1.1.1.1]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 OK)",
	"AAA111: to=<b@corp.com>, relay=none, delay=1, delays=0/0/1/0, dsn=4.4.1, status=deferred (connect timed out)",
}

func TestCommitIncludesPositionAndState(t *testing.T) {
	sink := &flakySink{}
	if err := newCollector(sink, 100).Run(context.Background(), entries(mail...)); err != nil {
		t.Fatal(err)
	}
	if len(sink.batches) != 1 {
		t.Fatalf("batches = %d", len(sink.batches))
	}
	b := sink.batches[0]
	if len(b.Events) != 2 || b.Position.Cursor != "c3" || len(b.Upserts) != 1 || b.Upserts[0].QueueID != "AAA111" {
		t.Errorf("batch = %+v", b)
	}
}

func TestRetryKeepsEverything(t *testing.T) {
	sink := &flakySink{fail: 2}
	start := time.Now()
	if err := newCollector(sink, 100).Run(context.Background(), entries(mail...)); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 3 || len(sink.batches) != 1 {
		t.Fatalf("calls=%d batches=%d", sink.calls, len(sink.batches))
	}
	if b := sink.batches[0]; len(b.Events) != 2 || len(b.Upserts) != 1 || b.Position.Cursor != "c3" {
		t.Errorf("retried batch lost data: %+v", b)
	}
	if time.Since(start) < 3*time.Second { // 1s + 2s backoff
		t.Error("no backoff between retries")
	}
}

func TestBatchSize(t *testing.T) {
	sink := &flakySink{}
	if err := newCollector(sink, 1).Run(context.Background(), entries(mail...)); err != nil {
		t.Fatal(err)
	}
	// One commit per event, then the final commit for the trailing position.
	if len(sink.batches) < 2 || len(sink.batches[0].Events) != 1 {
		t.Errorf("batches = %+v", sink.batches)
	}
}

func TestBackfillDoesNotTouchLiveState(t *testing.T) {
	sink := &flakySink{}
	c := newCollector(sink, 100)
	c.opt.Live = false
	if err := c.Run(context.Background(), entries(mail...)); err != nil {
		t.Fatal(err)
	}
	b := sink.batches[0]
	if !b.Position.IsZero() || len(b.Upserts)+len(b.Deletes) != 0 || len(b.Events) != 2 {
		t.Errorf("backfill batch = %+v", b)
	}
}

func TestCancelCommitsProcessedLines(t *testing.T) {
	sink := &flakySink{}
	ch := make(chan journal.Entry, 10)
	ch <- journal.Entry{Cursor: "c0", Time: time.Now(), Message: mail[2]}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- newCollector(sink, 100).Run(ctx, ch) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(sink.batches) != 1 || sink.batches[0].Position.Cursor != "c0" {
		t.Errorf("final commit missing: %+v", sink.batches)
	}
}
