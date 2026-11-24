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

package postfix

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	p := NewParser("X-Ref-ID")

	tests := []struct {
		name string
		msg  string
		want Record
	}{
		{
			name: "reference header via WARN, SMTP client",
			msg:  "4bZ7Kq1XyHz9vNP: warning: header X-Ref-ID: 090920260000002634477 from localhost[127.0.0.1]; from=<noreply@example.com> to=<a@gmail.com> proto=ESMTP helo=<app01>",
			want: Record{Kind: KindHeader, QueueID: "4bZ7Kq1XyHz9vNP", HeaderValue: "090920260000002634477", From: "noreply@example.com", HasFrom: true},
		},
		{
			name: "reference header via INFO, local pickup, lower-case name",
			msg:  "3F2A11C0D4: info: header x-ref-id: REF-1 from local; from=<root> to=<b@corp.com>",
			want: Record{Kind: KindHeader, QueueID: "3F2A11C0D4", HeaderValue: "REF-1", From: "root", HasFrom: true},
		},
		{
			name: "other header is ignored",
			msg:  "3F2A11C0D4: warning: header X-Mailer: app from local; from=<root> to=<b@corp.com>",
			want: Record{},
		},
		{
			name: "subject that itself contains ' from '",
			msg:  "3F2A11C0D4: warning: header Subject: hello from bob from localhost[127.0.0.1]; from=<root> to=<b@corp.com> proto=ESMTP helo=<app01>",
			want: Record{Kind: KindSubject, QueueID: "3F2A11C0D4", HeaderValue: "hello from bob", From: "root", HasFrom: true},
		},
		{
			name: "subject, MIME encoded",
			msg:  "3F2A11C0D4: warning: header Subject: =?UTF-8?B?4LCk4LGG4LCy4LGB4LCX4LGB?= from local; from=<root> to=<b@corp.com>",
			want: Record{Kind: KindSubject, QueueID: "3F2A11C0D4", HeaderValue: "తెలుగు", From: "root", HasFrom: true},
		},
		{
			name: "subject, folded into two encoded words",
			msg:  "3F2A11C0D4: warning: header Subject: =?UTF-8?Q?caf=C3=A9?=? =?UTF-8?Q?_menu?= from local; from=<root> to=<b@corp.com>",
			want: Record{Kind: KindSubject, QueueID: "3F2A11C0D4", HeaderValue: "café menu", From: "root", HasFrom: true},
		},
		{
			name: "subject, empty",
			msg:  "3F2A11C0D4: warning: header Subject:  from local; from=<root> to=<b@corp.com>",
			want: Record{Kind: KindSubject, QueueID: "3F2A11C0D4", From: "root", HasFrom: true},
		},
		{
			name: "qmgr queued",
			msg:  "4bZ7Kq1XyHz9vNP: from=<noreply@example.com>, size=2140, nrcpt=2 (queue active)",
			want: Record{Kind: KindQueued, QueueID: "4bZ7Kq1XyHz9vNP", From: "noreply@example.com", HasFrom: true},
		},
		{
			name: "qmgr queued, null sender",
			msg:  "4bZ7Kq1XyHz9vNQ: from=<>, size=3810, nrcpt=1 (queue active)",
			want: Record{Kind: KindQueued, QueueID: "4bZ7Kq1XyHz9vNQ", From: "", HasFrom: true},
		},
		{
			name: "sent",
			msg:  "4bZ7Kq1XyHz9vNP: to=<a@gmail.com>, relay=gmail-smtp-in.l.google.com[142.250.4.27]:25, delay=1.2, delays=0.01/0/0.5/0.7, dsn=2.0.0, status=sent (250 2.0.0 OK  1700000000 x1si123.45 - gsmtp)",
			want: Record{Kind: KindDelivery, QueueID: "4bZ7Kq1XyHz9vNP", To: "a@gmail.com", Relay: "gmail-smtp-in.l.google.com[142.250.4.27]:25", DSN: "2.0.0", Status: StatusSent, Text: "250 2.0.0 OK  1700000000 x1si123.45 - gsmtp"},
		},
		{
			name: "deferred with orig_to and conn_use",
			msg:  "4bZ7Kq1XyHz9vNP: to=<b@corp.com>, orig_to=<sales@corp.com>, relay=mx.corp.com[203.0.113.9]:25, conn_use=2, delay=3.1, delays=0/0/1/2.1, dsn=4.7.0, status=deferred (host mx.corp.com[203.0.113.9] said: 421 4.7.0 Try again later (in reply to RCPT TO command))",
			want: Record{Kind: KindDelivery, QueueID: "4bZ7Kq1XyHz9vNP", To: "b@corp.com", OrigTo: "sales@corp.com", Relay: "mx.corp.com[203.0.113.9]:25", DSN: "4.7.0", Status: StatusDeferred, Text: "host mx.corp.com[203.0.113.9] said: 421 4.7.0 Try again later (in reply to RCPT TO command)"},
		},
		{
			name: "deferred, connection timeout",
			msg:  "4bZ7Kq1XyHz9vNP: to=<c@slow.example>, relay=none, delay=30, delays=0/0/30/0, dsn=4.4.1, status=deferred (connect to mx.slow.example[198.51.100.7]:25: Connection timed out)",
			want: Record{Kind: KindDelivery, QueueID: "4bZ7Kq1XyHz9vNP", To: "c@slow.example", Relay: "none", DSN: "4.4.1", Status: StatusDeferred, Text: "connect to mx.slow.example[198.51.100.7]:25: Connection timed out"},
		},
		{
			name: "bounced",
			msg:  "4bZ7Kq1XyHz9vNP: to=<x@corp.com>, relay=mx.corp.com[203.0.113.9]:25, delay=0.9, delays=0/0/0.4/0.5, dsn=5.1.1, status=bounced (host mx.corp.com[203.0.113.9] said: 550 5.1.1 User unknown (in reply to RCPT TO command))",
			want: Record{Kind: KindDelivery, QueueID: "4bZ7Kq1XyHz9vNP", To: "x@corp.com", Relay: "mx.corp.com[203.0.113.9]:25", DSN: "5.1.1", Status: StatusBounced, Text: "host mx.corp.com[203.0.113.9] said: 550 5.1.1 User unknown (in reply to RCPT TO command)"},
		},
		{
			name: "expired",
			msg:  "4bZ7Kq1XyHz9vNP: from=<noreply@example.com>, status=expired, returned to sender",
			want: Record{Kind: KindExpired, QueueID: "4bZ7Kq1XyHz9vNP", From: "noreply@example.com", HasFrom: true},
		},
		{
			name: "removed",
			msg:  "4bZ7Kq1XyHz9vNP: removed",
			want: Record{Kind: KindRemoved, QueueID: "4bZ7Kq1XyHz9vNP"},
		},
		{name: "NOQUEUE reject", msg: "NOQUEUE: reject: RCPT from unknown[192.0.2.1]: 554 5.7.1 Relay access denied", want: Record{}},
		{
			name: "message-id",
			msg:  "4bZ7Kq1XyHz9vNP: message-id=<20261019.1@app01.example.com>",
			want: Record{Kind: KindMessageID, QueueID: "4bZ7Kq1XyHz9vNP", MessageID: "20261019.1@app01.example.com"},
		},
		{
			name: "message-id missing (client sent none)",
			msg:  "B6FA0634A3: message-id=<>",
			want: Record{Kind: KindMessageID, QueueID: "B6FA0634A3"},
		},
		{name: "resent-message-id is ignored", msg: "B6FA0634A3: resent-message-id=<x@y>", want: Record{}},
		{name: "plain warning", msg: "warning: hostname foo does not resolve to address 192.0.2.1", want: Record{}},
		{name: "empty", msg: "", want: Record{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Parse(tt.msg); got != tt.want {
				t.Errorf("Parse()\n got  %+v\n want %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeSubjectCutOffAtLogLimit(t *testing.T) {
	word := "=?UTF-8?B?4LCk4LGG4LCy4LGB4LCX4LGB?="
	var s string
	for len(s)+len(word)+1 < maxLoggedSubject {
		s += word + " "
	}
	complete := s + word[:maxLoggedSubject-len(s)] // cut inside the last word
	got := decodeSubject(complete)
	if want := "తెలుగు"; got[:len(want)] != want || strings.Contains(got, "=?") {
		t.Errorf("cut-off word was not dropped: %q", got)
	}

	// Below the limit, text that only looks like an encoded word is kept.
	if got, want := decodeSubject("is x=?y"), "is x=?y"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRecipient(t *testing.T) {
	if got := (Record{To: "a@x", OrigTo: "b@x"}).Recipient(); got != "b@x" {
		t.Errorf("with orig_to: got %q", got)
	}
	if got := (Record{To: "a@x"}).Recipient(); got != "a@x" {
		t.Errorf("without orig_to: got %q", got)
	}
}
