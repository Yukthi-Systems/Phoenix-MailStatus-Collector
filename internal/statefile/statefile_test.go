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

package statefile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

func TestSaveThenReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if pos, states := f.Load(); !pos.IsZero() || len(states) != 0 {
		t.Fatalf("first start should be empty: %+v %v", pos, states)
	}

	err = f.Save(&sink.Batch{
		Upserts: []tracker.State{
			{QueueID: "AAA", JSON: []byte(`{"from":"a@x"}`), LastSeen: now},
			{QueueID: "BBB", JSON: []byte(`{"from":"b@x"}`), LastSeen: now},
		},
		Position: journal.Position{Cursor: "c1", Time: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Save(&sink.Batch{Deletes: []string{"AAA"}, Position: journal.Position{Cursor: "c2", Time: now}}); err != nil {
		t.Fatal(err)
	}

	f, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pos, states := f.Load()
	if pos.Cursor != "c2" || !pos.Time.Equal(now) {
		t.Errorf("position = %+v", pos)
	}
	if len(states) != 1 || states[0].QueueID != "BBB" || string(states[0].JSON) != `{"from":"b@x"}` {
		t.Errorf("states = %+v", states)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error, starting empty would skip mail")
	}
}

func TestSaveWithNoChangesWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Save(&sink.Batch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should not exist: %v", err)
	}
}
