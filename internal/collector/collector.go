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

// Package collector wires the pipeline together:
//
//	journal entries -> postfix.Parser -> tracker.Tracker -> sink.Batch -> sink.Sink
//
// Everything runs on one goroutine, so the tracker needs no locking and the
// order of log lines is preserved. A batch (events + tracker changes + journal
// position) is handed to the sink as one unit and only cleared once the sink
// accepted it: after a crash the collector resumes from the last committed
// position with the matching tracker state. Re-read lines produce the same
// deterministic event IDs, so consumers can drop duplicates.
package collector

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/postfix"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

// Options controls batching and bookkeeping.
type Options struct {
	BatchSize     int           // commit after this many events
	FlushInterval time.Duration // ... or after this long
	StatsInterval time.Duration // log throughput this often (0 = never)
	ExpireEvery   time.Duration // run tracker.Expire this often
	Live          bool          // save journal position and tracker state (false for backfill / dry-run)
	ShutdownGrace time.Duration // time allowed for the final commit
	Output        string        // where results go, for the stats line ("rabbitmq", "stdout")
}

// Collector processes journal entries. Create with New.
type Collector struct {
	opt     Options
	parser  *postfix.Parser
	tracker *tracker.Tracker
	sink    sink.Sink
	log     *slog.Logger

	events   []mailstatus.Event
	pos      journal.Position // position of the last handled entry
	posDirty bool
	lastTime time.Time // time of the newest entry seen

	stats struct{ entries, events, written, commits int64 }
}

// New returns a collector that parses entries with p, tracks messages with t
// and commits batches to out.
func New(opt Options, p *postfix.Parser, t *tracker.Tracker, out sink.Sink, log *slog.Logger) *Collector {
	return &Collector{opt: opt, parser: p, tracker: t, sink: out, log: log}
}

// Run consumes entries until the channel is closed or ctx is cancelled, then
// commits what it has. It returns nil on a clean end of input, ctx.Err() on
// cancellation, or the commit error.
func (c *Collector) Run(ctx context.Context, entries <-chan journal.Entry) error {
	flush := time.NewTicker(c.opt.FlushInterval)
	defer flush.Stop()
	expire := time.NewTicker(c.opt.ExpireEvery)
	defer expire.Stop()
	var statsC <-chan time.Time
	if c.opt.StatsInterval > 0 {
		st := time.NewTicker(c.opt.StatsInterval)
		defer st.Stop()
		statsC = st.C
	}

	for {
		select {
		case e, ok := <-entries:
			if !ok {
				return c.final()
			}
			c.handle(e)
			if len(c.events) >= c.opt.BatchSize {
				if err := c.commit(ctx); err != nil {
					return c.stop(ctx, err)
				}
			}

		case <-flush.C:
			if err := c.commit(ctx); err != nil {
				return c.stop(ctx, err)
			}

		case <-expire.C:
			if n := c.tracker.Expire(c.lastTime); n > 0 {
				c.log.Warn("forgot stale messages (their 'removed' line was never seen)", "count", n)
			}

		case <-statsC:
			c.log.Info("stats", "output", c.opt.Output,
				"entries", c.stats.entries, "events", c.stats.events,
				"written", c.stats.written, "commits", c.stats.commits,
				"tracked_messages", c.tracker.Len(), "lag", lag(c.lastTime))

		case <-ctx.Done():
			return c.stop(ctx, ctx.Err())
		}
	}
}

// handle parses one journal entry, feeds it to the tracker and queues the
// resulting events for the next commit.
func (c *Collector) handle(e journal.Entry) {
	c.stats.entries++
	c.pos, c.posDirty = e.Position(), true
	if e.Time.After(c.lastTime) {
		c.lastTime = e.Time
	}
	r := c.parser.Parse(e.Message)
	if r.Kind == postfix.KindUnknown {
		return
	}
	ev := c.tracker.Handle(e, r)
	c.stats.events += int64(len(ev))
	c.events = append(c.events, ev...)
}

// commit writes the pending batch, retrying with backoff until it succeeds
// or ctx is cancelled. While it retries no new entries are read, so
// journalctl blocks on its pipe and the journal itself buffers - nothing is
// dropped and memory stays flat during a broker outage.
//
// Events, tracker changes and position are only cleared after a successful
// commit, so a later commit (e.g. the final one at shutdown) still has them.
func (c *Collector) commit(ctx context.Context) error {
	b := &sink.Batch{Events: c.events}
	if c.opt.Live {
		b.Upserts, b.Deletes = c.tracker.Snapshot()
		if c.posDirty {
			b.Position = c.pos
		}
	}
	if b.Empty() {
		return nil
	}

	backoff := time.Second
	for attempt := 1; ; attempt++ {
		n, err := c.sink.Commit(ctx, b)
		if err == nil {
			c.stats.written += n
			c.stats.commits++
			if attempt > 1 {
				c.log.Info("output write recovered", "attempts", attempt)
			}
			c.tracker.Committed()
			c.events = c.events[:0]
			c.posDirty = false
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Error("output write failed, retrying",
			"error", err, "attempt", attempt, "retry_in", backoff, "pending_events", len(b.Events))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// final commits the last batch at end of input (stdin mode).
func (c *Collector) final() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opt.ShutdownGrace)
	defer cancel()
	return c.commit(ctx)
}

// stop makes one bounded attempt to commit what was processed, then returns
// cause. Anything not committed is re-read from the journal on the next start.
func (c *Collector) stop(ctx context.Context, cause error) error {
	if ctx.Err() != nil {
		if err := c.final(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			c.log.Warn("final commit failed; uncommitted lines will be re-read on start", "error", err)
		}
	}
	return cause
}

// lag returns how far behind the wall clock t is, for the stats line.
func lag(t time.Time) string {
	if t.IsZero() {
		return "n/a"
	}
	return time.Since(t).Round(time.Second).String()
}
