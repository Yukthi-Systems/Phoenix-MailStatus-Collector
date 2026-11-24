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

package tracker

import (
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/postfix"
)

var (
	parser = postfix.NewParser("X-Ref-ID")
	quiet  = slog.New(slog.NewTextHandler(io.Discard, nil))
	t0     = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
)

// feed runs log lines through a tracker and returns all events.
func feed(tr *Tracker, lines ...string) []mailstatus.Event {
	var out []mailstatus.Event
	for i, l := range lines {
		e := journal.Entry{Cursor: "c" + strconv.Itoa(i), Time: t0.Add(time.Duration(i) * time.Second), Message: l}
		out = append(out, tr.Handle(e, parser.Parse(l))...)
	}
	return out
}

func newTracker() *Tracker {
	return New(Config{SuccessCode: "255", MaxAge: 24 * time.Hour}, quiet)
}

func TestDeferredThenSent(t *testing.T) {
	tr := newTracker()
	ev := feed(tr,
		"AAA111: warning: header X-Ref-ID: REF-1 from localhost[127.0.0.1]; from=<app@example.com> to=<a@corp.com> proto=ESMTP helo=<app>",
		"AAA111: from=<app@example.com>, size=100, nrcpt=2 (queue active)",
		"AAA111: to=<a@corp.com>, relay=mx.corp.com[203.0.113.9]:25, delay=1, delays=0/0/0/1, dsn=4.7.0, status=deferred (host mx.corp.com[203.0.113.9] said: 421 4.7.0 Try later (in reply to end of DATA command))",
		"AAA111: to=<b@gmail.com>, relay=gmail[142.250.4.27]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 2.0.0 OK)",
		"AAA111: from=<app@example.com>, size=100, nrcpt=2 (queue active)",
		"AAA111: to=<a@corp.com>, relay=mx.corp.com[203.0.113.9]:25, delay=301, delays=300/0/0/1, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as 4XYZ)",
		"AAA111: removed",
	)

	want := []struct{ to, status, code string }{
		{"a@corp.com", mailstatus.Deferred, "421"},
		{"b@gmail.com", mailstatus.Success, "255"},
		{"a@corp.com", mailstatus.Success, "255"},
	}
	if len(ev) != len(want) {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for i, w := range want {
		e := ev[i]
		if e.To != w.to || e.Status != w.status || e.ErrorCode != w.code {
			t.Errorf("event %d = %s/%s/%s, want %s/%s/%s", i, e.To, e.Status, e.ErrorCode, w.to, w.status, w.code)
		}
		if e.From != "app@example.com" || e.ReferenceID != "REF-1" || e.QueueID != "AAA111" {
			t.Errorf("event %d context = %q %q %q", i, e.From, e.ReferenceID, e.QueueID)
		}
	}
	if ev[1].Remarks != successRemarks {
		t.Errorf("success remarks = %q", ev[1].Remarks)
	}
	if tr.Len() != 0 {
		t.Errorf("message not forgotten after removed")
	}
}

func TestSubjectAndFirstSeenSurviveRestart(t *testing.T) {
	tr := newTracker()
	feed(tr,
		"SUB111: warning: header Subject: Invoice from localhost[127.0.0.1]; from=<app@x.in> to=<a@corp.com> proto=ESMTP helo=<app>",
		"SUB111: warning: header Subject: Attached mail from localhost[127.0.0.1]; from=<app@x.in> to=<a@corp.com> proto=ESMTP helo=<app>",
	)
	upserts, _ := tr.Snapshot()

	restarted := newTracker()
	if err := restarted.Restore(upserts); err != nil {
		t.Fatal(err)
	}
	ev := feed(restarted, "SUB111: to=<a@corp.com>, relay=mx[203.0.113.9]:25, delay=1, delays=0/0/0/1, dsn=4.7.0, status=deferred (try later)",
		"SUB111: from=<app@x.in>, status=expired, returned to sender")
	if len(ev) != 2 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for _, e := range ev {
		if e.Subject != "Invoice" || !e.QueuedAt.Equal(t0) || e.From != "app@x.in" {
			t.Errorf("subject %q, queued %v, from %q", e.Subject, e.QueuedAt, e.From)
		}
	}
}

// internalRelayLog is a real two-hop delivery through a DKIM-signing Fuglu.
var internalRelayLog = []string{
	"386283EAC9: client=relay-out.example.net[192.0.2.20]",
	"386283EAC9: warning: header Subject: Test Email to test flow of admin logs from relay-out.example.net[192.0.2.20]; from=<sender@example.net> to=<alice@outlook.com> proto=ESMTP helo=<relay-out>",
	"386283EAC9: warning: header X-YSPL-ID: f0767ff1-5ba4-4b43-8fad-63dee139795a from relay-out.example.net[192.0.2.20]; from=<sender@example.net> to=<alice@outlook.com> proto=ESMTP helo=<relay-out>",
	"386283EAC9: message-id=<>",
	"386283EAC9: from=<sender@example.net>, size=2045, nrcpt=1 (queue active)",
	"8FA5B3EAE6: client=dkim.example.net[192.0.2.30]",
	"8FA5B3EAE6: message-id=<>",
	"386283EAC9: to=<alice@outlook.com>, relay=dkim.example.net[192.0.2.30]:10029, delay=0.38, delays=0.07/0.02/0.06/0.23, dsn=2.0.0, status=sent (250 FUGLU REQUEUE(db3d606a345a4959a78922f1cb8ed1e2): 2.0.0 Ok: queued as 8FA5B3EAE6)",
	"8FA5B3EAE6: from=<sender@example.net>, size=3184, nrcpt=1 (queue active)",
	"386283EAC9: removed",
	"8FA5B3EAE6: to=<alice@outlook.com>, relay=outlook-com.olc.protection.outlook.com[52.101.40.101]:25, delay=4.3, delays=0.02/0.03/1.2/3, dsn=2.6.0, status=sent (250 2.6.0 <c98cf428@EXAMPLE02.prod.outlook.com> Queued mail for delivery -> 250 2.1.5)",
	"8FA5B3EAE6: removed",
}

// feedWith is feed with the reference header used on the real servers.
func feedWith(tr *Tracker, lines ...string) []mailstatus.Event {
	p := postfix.NewParser("X-YSPL-ID")
	var out []mailstatus.Event
	for i, l := range lines {
		e := journal.Entry{Cursor: "c" + strconv.Itoa(i), Time: t0.Add(time.Duration(i) * time.Second), Message: l}
		out = append(out, tr.Handle(e, p.Parse(l))...)
	}
	return out
}

func newInternalRelayTracker() *Tracker {
	return New(Config{SuccessCode: "250", InternalRelays: []string{"DKIM.example.net"}}, quiet)
}

func TestInternalRelayIsSkippedAndContextCarriedOver(t *testing.T) {
	tr := newInternalRelayTracker()
	ev := feedWith(tr, internalRelayLog...)
	if len(ev) != 1 {
		t.Fatalf("got %d events, want only the Outlook delivery: %+v", len(ev), ev)
	}
	e := ev[0]
	if e.QueueID != "8FA5B3EAE6" || e.Status != mailstatus.Success || e.To != "alice@outlook.com" {
		t.Errorf("event = %+v", e)
	}
	if e.ReferenceID != "f0767ff1-5ba4-4b43-8fad-63dee139795a" {
		t.Errorf("ReferenceID = %q", e.ReferenceID)
	}
	if e.Subject != "Test Email to test flow of admin logs" {
		t.Errorf("Subject = %q", e.Subject)
	}
	if e.From != "sender@example.net" {
		t.Errorf("From = %q", e.From)
	}
	if !e.QueuedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("QueuedAt = %v, want the first hop's first line", e.QueuedAt)
	}
	if tr.Len() != 0 {
		t.Errorf("%d messages still tracked", tr.Len())
	}
}

func TestInternalRelayHandOverSurvivesRestart(t *testing.T) {
	const handOverLine = 8 // through the internal relay's "queued as" line
	tr := newInternalRelayTracker()
	feedWith(tr, internalRelayLog[:handOverLine]...)
	upserts, _ := tr.Snapshot()

	restarted := newInternalRelayTracker()
	if err := restarted.Restore(upserts); err != nil {
		t.Fatal(err)
	}
	ev := feedWith(restarted, internalRelayLog[handOverLine:]...)
	if len(ev) != 1 || ev[0].Subject == "" || ev[0].ReferenceID == "" {
		t.Fatalf("events = %+v", ev)
	}
}

func TestInternalRelayFailuresAreStillReported(t *testing.T) {
	for status, want := range map[string]string{"deferred": mailstatus.Deferred, "bounced": mailstatus.Failure} {
		ev := feedWith(newInternalRelayTracker(),
			"AAA111: from=<a@x.in>, size=1, nrcpt=1 (queue active)",
			"AAA111: to=<b@y.com>, relay=dkim.example.net[192.0.2.30]:10029, delay=1, delays=0/0/0/1, status="+status+" (host dkim.example.net[192.0.2.30] said: 451 try later)",
		)
		if len(ev) != 1 || ev[0].Status != want {
			t.Errorf("%s: events = %+v", status, ev)
		}
	}
}

func TestInternalRelayDeferredThenAcceptedClearsPending(t *testing.T) {
	tr := newInternalRelayTracker()
	ev := feedWith(tr,
		"AAA111: from=<a@x.in>, size=1, nrcpt=1 (queue active)",
		"AAA111: to=<b@y.com>, relay=dkim.example.net[192.0.2.30]:10029, delay=1, delays=0/0/0/1, status=deferred (try later)",
		"AAA111: to=<b@y.com>, relay=dkim.example.net[192.0.2.30]:10029, delay=1, delays=0/0/0/1, status=sent (250 2.0.0 Ok: queued as BBB222)",
		"AAA111: from=<a@x.in>, status=expired, returned to sender",
	)
	if len(ev) != 1 || ev[0].Status != mailstatus.Deferred {
		t.Fatalf("no Failure expected once the relay accepted it: %+v", ev)
	}
}

func TestOtherRelaysAreNotSkipped(t *testing.T) {
	ev := feedWith(New(Config{SuccessCode: "250"}, quiet), internalRelayLog...)
	if len(ev) != 2 {
		t.Fatalf("with no internal relays configured, got %d events: %+v", len(ev), ev)
	}
}

func TestBouncedAndOrigTo(t *testing.T) {
	ev := feed(newTracker(),
		"BBB222: from=<app@x.in>, size=100, nrcpt=1 (queue active)",
		"BBB222: to=<real@corp.com>, orig_to=<alias@corp.com>, relay=mx[203.0.113.9]:25, delay=1, delays=0/0/0/1, dsn=5.1.1, status=bounced (host mx[203.0.113.9] said: 550 5.1.1 User unknown (in reply to RCPT TO command))",
	)
	if len(ev) != 1 {
		t.Fatalf("got %+v", ev)
	}
	if e := ev[0]; e.Status != mailstatus.Failure || e.ErrorCode != "550" || e.To != "alias@corp.com" {
		t.Errorf("got %+v", e)
	}
}

func TestExpired(t *testing.T) {
	tr := newTracker()
	ev := feed(tr,
		"CCC333: from=<app@x.in>, size=100, nrcpt=3 (queue active)",
		"CCC333: to=<z@slow.example>, relay=none, delay=30, delays=0/0/30/0, dsn=4.4.1, status=deferred (connect to mx.slow.example[198.51.100.7]:25: Connection timed out)",
		"CCC333: to=<y@grey.example>, relay=mx[203.0.113.5]:25, delay=1, delays=0/0/0/1, dsn=4.7.1, status=deferred (host mx[203.0.113.5] said: 450 4.7.1 Greylisted (in reply to RCPT TO command))",
		"CCC333: to=<ok@gmail.com>, relay=gmail[142.250.4.27]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 2.0.0 OK)",
		"CCC333: from=<app@x.in>, status=expired, returned to sender",
		"CCC333: removed",
	)
	// 2 deferred + 1 sent + 2 failures at expiry (sorted by recipient)
	if len(ev) != 5 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	if e := ev[0]; e.ErrorCode != DefaultDeferredCode {
		t.Errorf("timeout deferral code = %q, want %s (IP octets must not count)", e.ErrorCode, DefaultDeferredCode)
	}
	exp := ev[3:]
	if exp[0].To != "y@grey.example" || exp[0].Status != mailstatus.Failure || exp[0].ErrorCode != "450" {
		t.Errorf("expired[0] = %+v", exp[0])
	}
	if exp[1].To != "z@slow.example" || exp[1].ErrorCode != DefaultDeferredCode {
		t.Errorf("expired[1] = %+v", exp[1])
	}
	if exp[0].UID == exp[1].UID {
		t.Error("expired events share a UID")
	}
}

func TestNullSenderSkipped(t *testing.T) {
	ev := feed(newTracker(),
		"DDD444: from=<>, size=3000, nrcpt=1 (queue active)",
		"DDD444: to=<app@x.in>, relay=local, delay=0, delays=0/0/0/0, dsn=2.0.0, status=sent (delivered to mailbox)",
		"DDD444: removed",
	)
	if len(ev) != 0 {
		t.Errorf("notification produced events: %+v", ev)
	}
}

func TestUnknownSenderStillRecorded(t *testing.T) {
	ev := feed(newTracker(),
		"EEE555: to=<a@gmail.com>, relay=gmail[142.250.4.27]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 2.0.0 OK)",
	)
	if len(ev) != 1 || ev[0].From != "" {
		t.Errorf("got %+v", ev)
	}
}

func TestDeterministicUID(t *testing.T) {
	line := "FFF666: to=<a@gmail.com>, relay=g[1.1.1.1]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 OK)"
	a, b := feed(newTracker(), line), feed(newTracker(), line)
	if a[0].UID != b[0].UID {
		t.Errorf("same log line produced different UIDs: %s vs %s", a[0].UID, b[0].UID)
	}
}

func TestSnapshotRestore(t *testing.T) {
	tr := newTracker()
	feed(tr,
		"GGG777: warning: header X-Ref-ID: REF-9 from local; from=<app@x.in> to=<a@corp.com>",
		"GGG777: from=<app@x.in>, size=1, nrcpt=1 (queue active)",
		"GGG777: to=<a@corp.com>, relay=none, delay=1, delays=0/0/1/0, dsn=4.4.1, status=deferred (connect timed out)",
		"HHH888: from=<app@x.in>, size=1, nrcpt=1 (queue active)",
		"HHH888: removed",
	)
	ups, dels := tr.Snapshot()
	if len(ups) != 1 || ups[0].QueueID != "GGG777" || len(dels) != 1 || dels[0] != "HHH888" {
		t.Fatalf("snapshot = %+v / %v", ups, dels)
	}
	if u, d := tr.Snapshot(); len(u) != 1 || len(d) != 1 {
		t.Errorf("uncommitted changes lost on second snapshot: %v %v", u, d)
	}
	tr.Committed()
	if u, d := tr.Snapshot(); len(u)+len(d) != 0 {
		t.Errorf("snapshot after Committed not empty: %v %v", u, d)
	}

	// A new process continues the message where the old one stopped.
	tr2 := newTracker()
	if err := tr2.Restore(ups); err != nil {
		t.Fatal(err)
	}
	ev := tr2.Handle(journal.Entry{Cursor: "x", Time: t0.Add(time.Hour)},
		parser.Parse("GGG777: from=<app@x.in>, status=expired, returned to sender"))
	if len(ev) != 1 || ev[0].ReferenceID != "REF-9" || ev[0].From != "app@x.in" || ev[0].Status != mailstatus.Failure {
		t.Errorf("after restore: %+v", ev)
	}
}

func TestExpire(t *testing.T) {
	tr := newTracker()
	feed(tr, "III999: from=<app@x.in>, size=1, nrcpt=1 (queue active)")
	if n := tr.Expire(t0.Add(time.Hour)); n != 0 {
		t.Errorf("expired %d fresh messages", n)
	}
	if n := tr.Expire(t0.Add(48 * time.Hour)); n != 1 || tr.Len() != 0 {
		t.Errorf("expired %d, len %d", n, tr.Len())
	}
}

func TestReplyCode(t *testing.T) {
	for text, want := range map[string]string{
		"host mx[203.0.113.9] said: 550 5.1.1 User unknown":       "550",
		"host mx[203.0.113.9] said: 421-4.7.0 Try again":          "421",
		"250 2.0.0 Ok: queued as ABC":                             "250",
		"connect to mx[203.0.113.9]:25: Connection timed out":     "def",
		"Host or domain name not found. Name service error for x": "def",
		"": "def",
	} {
		if got := ReplyCode(text, "def"); got != want {
			t.Errorf("ReplyCode(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestNodeInEvents(t *testing.T) {
	line := "JJJ000: to=<a@gmail.com>, relay=g[1.1.1.1]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 OK)"
	mx1 := New(Config{Node: "mx1", SuccessCode: "255"}, quiet)
	mx2 := New(Config{Node: "mx2", SuccessCode: "255"}, quiet)
	a, b := feed(mx1, line), feed(mx2, line)
	if a[0].Server != "mx1" || b[0].Server != "mx2" {
		t.Errorf("server = %q / %q", a[0].Server, b[0].Server)
	}
	if a[0].UID == b[0].UID {
		t.Error("same log line on two servers produced the same event_uid")
	}
}

func TestMessageID(t *testing.T) {
	tr := newTracker()
	ev := feed(tr,
		"KKK111: message-id=<20261021.42@app01.example.com>",
		"KKK111: from=<app@x.in>, size=1, nrcpt=2 (queue active)",
		"KKK111: to=<a@gmail.com>, relay=g[1.1.1.1]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 OK)",
		"KKK111: to=<b@corp.com>, relay=none, delay=1, delays=0/0/1/0, dsn=4.4.1, status=deferred (connect timed out)",
		"KKK111: from=<app@x.in>, status=expired, returned to sender",
		"LLL222: message-id=<>",
		"LLL222: from=<app@x.in>, size=1, nrcpt=1 (queue active)",
		"LLL222: to=<c@gmail.com>, relay=g[1.1.1.1]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 OK)",
	)
	if len(ev) != 4 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	for _, e := range ev[:3] { // sent, deferred, expired failure: all carry it
		if e.MessageID != "20261021.42@app01.example.com" {
			t.Errorf("%s/%s: message_id = %q", e.To, e.Status, e.MessageID)
		}
	}
	if ev[3].MessageID != "" {
		t.Errorf("mail without Message-ID: got %q", ev[3].MessageID)
	}

	// Kept across a restart.
	ups, _ := tr.Snapshot()
	tr2 := newTracker()
	if err := tr2.Restore(ups); err != nil {
		t.Fatal(err)
	}
	got := tr2.Handle(journal.Entry{Cursor: "z", Time: t0.Add(time.Hour)},
		parser.Parse("LLL222: to=<d@gmail.com>, relay=g[1.1.1.1]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 OK)"))
	if len(got) != 1 || got[0].MessageID != "" {
		t.Errorf("after restore: %+v", got)
	}
}

func TestMXResponseIsKept(t *testing.T) {
	ev := feed(newTracker(),
		"A50BE3EAF7: from=<sender@example.net>, size=2985, nrcpt=2 (queue active)",
		"A50BE3EAF7: to=<bob@gmail.com>, relay=gmail-smtp-in.l.google.com[74.125.131.27]:25, delay=0.49, delays=0.02/0/0.05/0.42, dsn=2.0.0, status=sent (250 2.0.0 OK  1794559714 2adb3069b0e04-5c1712d7445si1390237e87.27 - gsmtp)",
		"A50BE3EAF7: to=<alice@outlook.com>, relay=outlook-com.olc.protection.outlook.com[52.101.103.161]:25, delay=6.7, delays=0.02/0/0.95/5.7, dsn=2.6.0, status=sent (250 2.6.0 <app01.example.com@localdomain.com> [InternalId=00000000000000, Hostname=EXAMPLE01.prod.outlook.com] 12267 bytes in 0.356, 33.628 KB/sec Queued mail for delivery)",
		"A50BE3EAF7: removed",
	)
	if len(ev) != 2 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	if e := ev[0]; e.Relay != "gmail-smtp-in.l.google.com[74.125.131.27]:25" || e.DSN != "2.0.0" ||
		e.Response != "250 2.0.0 OK  1794559714 2adb3069b0e04-5c1712d7445si1390237e87.27 - gsmtp" {
		t.Errorf("gmail = %q %q %q", e.Relay, e.DSN, e.Response)
	}
	if e := ev[1]; e.DSN != "2.6.0" || e.Remarks != successRemarks ||
		e.Response != "250 2.6.0 <app01.example.com@localdomain.com> [InternalId=00000000000000, Hostname=EXAMPLE01.prod.outlook.com] 12267 bytes in 0.356, 33.628 KB/sec Queued mail for delivery" {
		t.Errorf("outlook = %q %q %q", e.DSN, e.Remarks, e.Response)
	}
}

func TestMXResponseOfBounceAndExpiry(t *testing.T) {
	tr := newTracker()
	ev := feed(tr,
		"DDD444: from=<app@x.in>, size=100, nrcpt=2 (queue active)",
		"DDD444: to=<nobody@gmail.com>, relay=gmail[142.250.4.27]:25, delay=1, delays=0/0/0/1, dsn=5.1.1, status=bounced (host gmail[142.250.4.27] said: 550 5.1.1 The email account does not exist (in reply to RCPT TO command))",
		"DDD444: to=<y@grey.example>, relay=mx[203.0.113.5]:25, delay=1, delays=0/0/0/1, dsn=4.7.1, status=deferred (host mx[203.0.113.5] said: 450 4.7.1 Greylisted (in reply to RCPT TO command))",
	)
	// The deferral must survive a restart to show up on the expiry.
	ups, _ := tr.Snapshot()
	restarted := newTracker()
	states := make([]State, len(ups))
	copy(states, ups)
	if err := restarted.Restore(states); err != nil {
		t.Fatal(err)
	}
	ev = append(ev, feed(restarted, "DDD444: from=<app@x.in>, status=expired, returned to sender")...)
	if len(ev) != 3 {
		t.Fatalf("got %d events: %+v", len(ev), ev)
	}
	if e := ev[0]; e.DSN != "5.1.1" || e.Relay != "gmail[142.250.4.27]:25" || e.Response != e.Remarks {
		t.Errorf("bounce = %+v", e)
	}
	if e := ev[2]; !e.Expired || e.DSN != "4.7.1" || e.Relay != "mx[203.0.113.5]:25" ||
		e.Response != "host mx[203.0.113.5] said: 450 4.7.1 Greylisted (in reply to RCPT TO command)" {
		t.Errorf("expired = %+v", e)
	}
}
