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

// Package journal reads systemd journal entries as produced by
// `journalctl -o json`.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Entry is the subset of a journal entry the collector needs.
type Entry struct {
	Cursor     string    // __CURSOR, opaque resume position
	Time       time.Time // __REALTIME_TIMESTAMP
	Identifier string    // SYSLOG_IDENTIFIER, e.g. "postfix/smtp"
	Message    string    // MESSAGE
}

// Position is where reading resumes. Cursor is preferred; Time is the
// fallback when the cursor no longer exists (journal vacuumed or rotated away).
type Position struct {
	Cursor string
	Time   time.Time
}

// IsZero reports whether no position has been saved yet.
func (p Position) IsZero() bool { return p.Cursor == "" && p.Time.IsZero() }

// Position returns the resume position just after e.
func (e Entry) Position() Position { return Position{Cursor: e.Cursor, Time: e.Time} }

// rawEntry holds only the fields the collector reads; encoding/json skips the
// rest (~20 per entry) without allocating them.
type rawEntry struct {
	Cursor     json.RawMessage `json:"__CURSOR"`
	Realtime   json.RawMessage `json:"__REALTIME_TIMESTAMP"`
	Identifier json.RawMessage `json:"SYSLOG_IDENTIFIER"`
	Message    json.RawMessage `json:"MESSAGE"`
}

// Decode parses one line of `journalctl -o json` output.
func Decode(line []byte) (Entry, error) {
	var raw rawEntry
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{}, fmt.Errorf("decode journal entry: %w", err)
	}

	var e Entry
	var err error
	if e.Cursor, err = field("__CURSOR", raw.Cursor); err != nil {
		return Entry{}, err
	}
	if e.Cursor == "" {
		return Entry{}, errors.New("journal entry has no __CURSOR")
	}
	ts, err := field("__REALTIME_TIMESTAMP", raw.Realtime)
	if err != nil {
		return Entry{}, err
	}
	usec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Entry{}, fmt.Errorf("bad __REALTIME_TIMESTAMP %q: %w", ts, err)
	}
	e.Time = time.UnixMicro(usec)
	if e.Identifier, err = field("SYSLOG_IDENTIFIER", raw.Identifier); err != nil {
		return Entry{}, err
	}
	if e.Message, err = field("MESSAGE", raw.Message); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// field returns a journal field as a string. journalctl encodes a value as a
// JSON string, as an array of byte values when it is not valid UTF-8 (or has
// control characters), as null when it is too large, and as an array of those
// when the field occurs more than once (the first one is used).
// Byte arrays are returned as valid UTF-8: see toUTF8.
func field(name string, v json.RawMessage) (string, error) {
	if len(v) == 0 || string(v) == "null" {
		return "", nil
	}
	if v[0] == '"' {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", fmt.Errorf("journal field %s: %w", name, err)
		}
		return s, nil
	}
	var nums []int
	if err := json.Unmarshal(v, &nums); err == nil {
		b := make([]byte, len(nums))
		for i, n := range nums {
			b[i] = byte(n)
		}
		return toUTF8(b), nil
	}
	var multi []json.RawMessage
	if err := json.Unmarshal(v, &multi); err == nil && len(multi) > 0 {
		return field(name, multi[0])
	}
	return "", fmt.Errorf("journal field %s: unsupported encoding %.40s", name, v)
}

// toUTF8 makes b valid UTF-8 for the JSON output: valid sequences are kept,
// every other byte is read as Latin-1 (remote SMTP replies often use it, e.g.
// 0xb4 is "´"), and NUL bytes are dropped.
func toUTF8(b []byte) string {
	if utf8.Valid(b) && !strings.ContainsRune(string(b), 0) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + 8)
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size == 1:
			sb.WriteRune(rune(b[0]))
		case r != 0:
			sb.Write(b[:size])
		}
		b = b[size:]
	}
	return sb.String()
}
