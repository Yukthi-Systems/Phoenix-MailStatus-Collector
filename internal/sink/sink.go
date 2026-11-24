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

// Package sink defines the unit of work the collector hands to its output
// (a Batch) and the interface an output implements (Sink).
//
// It has no dependencies on any particular output, so the collector, the
// RabbitMQ publisher, the state file and test doubles can all share it
// without importing one another.
package sink

import (
	"context"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

// Batch is everything the collector commits at once: the delivery events
// produced since the last commit, the tracker changes that go with them and
// the journal position they were read up to.
//
// Upserts, Deletes and Position are only set when the collector follows the
// journal live; in backfill and dry-run mode they are empty.
type Batch struct {
	Events   []mailstatus.Event
	Upserts  []tracker.State  // in-flight messages that changed
	Deletes  []string         // queue IDs no longer tracked
	Position journal.Position // zero: do not save (backfill mode)
}

// Empty reports whether the batch carries nothing to write or save.
func (b *Batch) Empty() bool {
	return len(b.Events) == 0 && len(b.Upserts) == 0 && len(b.Deletes) == 0 && b.Position.IsZero()
}

// Sink receives batches from the collector.
//
// Commit must be safe to call again with the same batch after it returned an
// error: the collector retries a failed batch unchanged, with backoff, before
// it reads any further input. It returns the number of events written.
type Sink interface {
	Commit(ctx context.Context, b *Batch) (int64, error)
}
