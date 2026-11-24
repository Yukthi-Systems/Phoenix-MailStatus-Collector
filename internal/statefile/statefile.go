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

// Package statefile keeps the collector's journal position and in-flight
// messages in a local JSON file, rewritten atomically after every batch the
// broker confirmed.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

// File is the state of one collector. Not safe for concurrent use.
type File struct {
	path string
	doc  document
}

// document is the on-disk JSON layout of the state file.
type document struct {
	Cursor   string             `json:"cursor,omitempty"`
	Time     time.Time          `json:"time,omitzero"`
	Messages map[string]message `json:"messages"`
}

// message is one tracked message in the state file.
type message struct {
	State    json.RawMessage `json:"state"`
	LastSeen time.Time       `json:"last_seen"`
}

// Open loads the state at path. A missing file is a first start. An unreadable
// one is an error: starting empty would silently skip mail logged in between.
func Open(path string) (*File, error) {
	f := &File{path: path, doc: document{Messages: make(map[string]message)}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &f.doc); err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	if f.doc.Messages == nil {
		f.doc.Messages = make(map[string]message)
	}
	return f, nil
}

// Load returns the saved journal position and tracked messages.
func (f *File) Load() (journal.Position, []tracker.State) {
	states := make([]tracker.State, 0, len(f.doc.Messages))
	for qid, m := range f.doc.Messages {
		states = append(states, tracker.State{QueueID: qid, JSON: m.State, LastSeen: m.LastSeen})
	}
	return journal.Position{Cursor: f.doc.Cursor, Time: f.doc.Time}, states
}

// Save applies the batch's state changes and replaces the file atomically, so
// a crash leaves either the old state or the new one. Applying the same
// changes again is harmless, so a failed Save can be retried.
func (f *File) Save(b *sink.Batch) error {
	if len(b.Upserts) == 0 && len(b.Deletes) == 0 && b.Position.IsZero() {
		return nil
	}
	for _, s := range b.Upserts {
		f.doc.Messages[s.QueueID] = message{State: s.JSON, LastSeen: s.LastSeen}
	}
	for _, qid := range b.Deletes {
		delete(f.doc.Messages, qid)
	}
	if !b.Position.IsZero() {
		f.doc.Cursor, f.doc.Time = b.Position.Cursor, b.Position.Time
	}

	data, err := json.Marshal(f.doc)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := writeAtomic(f.path, data); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// writeAtomic writes through a temporary file (mode 0600: the state holds
// addresses and subjects) and syncs it, so the rename never exposes partial data.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly after the rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
