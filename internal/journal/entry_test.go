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
	"testing"
	"time"
)

func TestDecode(t *testing.T) {
	line := `{"__CURSOR":"s=abc;i=1","__REALTIME_TIMESTAMP":"1760860800123456","SYSLOG_IDENTIFIER":"postfix/smtp","MESSAGE":"ABCDEF1234: removed","_PID":"901"}`
	e, err := Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{
		Cursor:     "s=abc;i=1",
		Time:       time.UnixMicro(1760860800123456),
		Identifier: "postfix/smtp",
		Message:    "ABCDEF1234: removed",
	}
	if !e.Time.Equal(want.Time) || e.Cursor != want.Cursor || e.Identifier != want.Identifier || e.Message != want.Message {
		t.Errorf("got %+v, want %+v", e, want)
	}
}

func TestDecodeByteArrayMessage(t *testing.T) {
	// "hi\x01" is not printable, so journalctl emits it as a byte array.
	line := `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","SYSLOG_IDENTIFIER":["postfix/qmgr","dup"],"MESSAGE":[104,105,1]}`
	e, err := Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if e.Message != "hi\x01" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Identifier != "postfix/qmgr" {
		t.Errorf("identifier = %q", e.Identifier)
	}
}

func TestDecodeInvalidUTF8(t *testing.T) {
	// Latin-1 "it´s" (0xb4), a valid UTF-8 "é" (0xc3 0xa9), and a NUL byte.
	line := `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":[105,116,180,115,32,195,169,0,33]}`
	e, err := Decode([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if e.Message != "it´s é!" {
		t.Errorf("message = %q", e.Message)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, line := range []string{
		`not json`,
		`{"__REALTIME_TIMESTAMP":"1","MESSAGE":"x"}`,
		`{"__CURSOR":"c","__REALTIME_TIMESTAMP":"soon","MESSAGE":"x"}`,
	} {
		if _, err := Decode([]byte(line)); err == nil {
			t.Errorf("Decode(%s): expected error", line)
		}
	}
}

func TestArgs(t *testing.T) {
	r := &Reader{Identifiers: []string{"postfix/smtp", "postfix/qmgr"}}
	got := r.args(Position{}, true)
	want := []string{"--output=json", "--no-pager", "--quiet", "--follow",
		"--identifier=postfix/smtp", "--identifier=postfix/qmgr", "--lines=0"}
	if len(got) != len(want) {
		t.Fatalf("args = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
	if a := r.args(Position{Cursor: "s=1"}, true); a[len(a)-1] != "--after-cursor=s=1" {
		t.Errorf("cursor args = %v", a)
	}
	if a := r.args(Position{Time: time.Unix(1700000000, 0)}, true); a[len(a)-1] != "--since=@1700000000" {
		t.Errorf("time args = %v", a)
	}
}
