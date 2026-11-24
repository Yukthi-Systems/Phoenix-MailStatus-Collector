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

package rabbitmq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
)

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func event(uid, to, status, remarks string, at time.Duration) mailstatus.Event {
	return mailstatus.Event{
		UID: uid, From: "app@example.in", To: to, Time: t0.Add(at), ReferenceID: "YSPL-1", MessageID: "m1@app",
		Status: status, Remarks: remarks, ErrorCode: "250", QueueID: "AAA111", Server: "node1",
		Subject: "Invoice", QueuedAt: t0.Add(-time.Minute),
	}
}

func TestGroupMergesRecipientsWithTheSameOutcome(t *testing.T) {
	msgs := Group([]mailstatus.Event{
		event("u1", "a@Gmail.com", mailstatus.Success, "ok", 0),
		event("u2", "b@corp.com", mailstatus.Success, "ok", 2*time.Second),
		event("u3", "c@gmail.com", mailstatus.Success, "ok", time.Second),
	}, "mail_status", nil)

	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(msgs), msgs)
	}
	d := msgs[0].Details
	if want := []string{"a@Gmail.com", "b@corp.com", "c@gmail.com"}; !slices.Equal(d.ToEmailIds, want) {
		t.Errorf("ToEmailIds = %v", d.ToEmailIds)
	}
	if want := []string{"corp.com", "example.in", "gmail.com"}; !slices.Equal(d.EmailDomains, want) { // with the sender's, sorted, like Fuglu
		t.Errorf("EmailDomains = %v", d.EmailDomains)
	}
	if d.EUID != "YSPL-1" || d.MessageID != "m1@app" || d.Subject != "Invoice" || d.FromEmailId != "app@example.in" || d.Type != "mail_status" {
		t.Errorf("details = %+v", d)
	}
	if d.LogTimestamp != "2026-11-07T10:00:02+00:00" {
		t.Errorf("LogTimestamp = %q, want the newest result", d.LogTimestamp)
	}
	if d.EmailTimeStamp != "2026-11-07T09:59:00+00:00" {
		t.Errorf("EmailTimeStamp = %q", d.EmailTimeStamp)
	}
	uids := d.MetaData["event_uids"].(map[string]string)
	if len(uids) != 3 || uids["u2"] != "b@corp.com" {
		t.Errorf("event_uids = %v", uids)
	}
}

func TestGroupKeepsDifferentOutcomesAndMailsApart(t *testing.T) {
	other := event("u4", "d@x.com", mailstatus.Success, "ok", 0)
	other.QueueID = "BBB222"
	msgs := Group([]mailstatus.Event{
		event("u1", "a@x.com", mailstatus.Success, "ok", 0),
		event("u2", "b@x.com", mailstatus.Deferred, "try later", 0),
		event("u3", "c@x.com", mailstatus.Deferred, "greylisted", 0),
		other,
	}, "t", nil)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
}

func TestGroupListsARepeatedRecipientOnceAndKeepsItsEvents(t *testing.T) {
	// An alias expanding to several mailboxes logs one orig_to several times.
	msgs := Group([]mailstatus.Event{
		event("u1", "sales@x.com", mailstatus.Success, "ok", 0),
		event("u2", "sales@x.com", mailstatus.Success, "ok", 0),
	}, "t", nil)
	d := msgs[0].Details
	if len(d.ToEmailIds) != 1 || len(d.MetaData["event_uids"].(map[string]string)) != 2 {
		t.Errorf("details = %+v", d)
	}
}

func TestStatusValues(t *testing.T) {
	expired := event("u4", "a@x.com", mailstatus.Failure, "gave up", 0)
	expired.Expired = true
	tests := []struct {
		event mailstatus.Event
		want  string
	}{
		{event("u1", "a@x.com", mailstatus.Success, "ok", 0), StatusDelivered},
		{event("u2", "a@x.com", mailstatus.Deferred, "try later", 0), StatusDeferred},
		{event("u3", "a@x.com", mailstatus.Failure, "user unknown", 0), StatusBounced},
		{expired, StatusExpired},
	}
	for _, tt := range tests {
		if got := Group([]mailstatus.Event{tt.event}, "Outgoing", nil)[0].Details.Status; got != tt.want {
			t.Errorf("%s (expired=%v): got %q, want %q", tt.event.Status, tt.event.Expired, got, tt.want)
		}
	}
}

func TestTimesInTheConfiguredZone(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	d := Group([]mailstatus.Event{event("u1", "a@x.com", mailstatus.Success, "ok", 0)}, "t", ist)[0].Details
	if d.LogTimestamp != "2026-11-07T15:30:00+05:30" || d.EmailTimeStamp != "2026-11-07T15:29:00+05:30" {
		t.Errorf("timestamps %q, %q: want +05:30 like Fuglu", d.LogTimestamp, d.EmailTimeStamp)
	}
}

func TestMissingSubjectAndQueueTime(t *testing.T) {
	e := event("u1", "a@x.com", mailstatus.Success, "ok", 0)
	e.Subject, e.QueuedAt = "", time.Time{}
	d := Group([]mailstatus.Event{e}, "t", nil)[0].Details
	if d.Subject != "No Subject" {
		t.Errorf("Subject = %q, want the No Subject placeholder", d.Subject)
	}
	if d.EmailTimeStamp != "2026-11-07T10:00:00+00:00" {
		t.Errorf("EmailTimeStamp = %q, want the event time", d.EmailTimeStamp)
	}
}

func TestMessageIDIsStableAcrossRetries(t *testing.T) {
	events := []mailstatus.Event{event("u1", "a@x.com", mailstatus.Success, "ok", 0)}
	if a, b := Group(events, "t", nil)[0].ID, Group(events, "t", nil)[0].ID; a != b || a == "" {
		t.Errorf("IDs %q and %q", a, b)
	}
}

func TestBodyMatchesTheContract(t *testing.T) {
	body, err := json.Marshal(Group([]mailstatus.Event{event("u1", "a@x.com", mailstatus.Success, "ok", 0)}, "t", nil)[0].Details)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"euid", "message_id", "from_email_id", "to_email_ids", "email_domains", "type", "status",
		"status_description", "subject", "log_timestamp", "email_timestamp", "meta_data"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %q in %s", key, body)
		}
	}
}

type fakePublisher struct {
	calls int
	err   error
}

func (f *fakePublisher) Publish(context.Context, []mailstatus.Event) error {
	f.calls++
	return f.err
}

type fakeState struct {
	calls int
	err   error
}

func (f *fakeState) Save(*sink.Batch) error {
	f.calls++
	return f.err
}

func TestSinkSavesStateOnlyAfterPublishing(t *testing.T) {
	pub, state := &fakePublisher{err: errors.New("broker down")}, &fakeState{}
	s := &Sink{Publisher: pub, State: state}
	if _, err := s.Commit(context.Background(), &sink.Batch{}); err == nil {
		t.Fatal("expected the publish error")
	}
	if state.calls != 0 {
		t.Error("state saved although publishing failed: the mail would be lost after a restart")
	}
}

func TestSinkDoesNotRepublishWhenOnlyTheStateSaveFailed(t *testing.T) {
	pub, state := &fakePublisher{}, &fakeState{err: errors.New("disk full")}
	s := &Sink{Publisher: pub, State: state}
	b := &sink.Batch{Events: []mailstatus.Event{{UID: "u1"}}}

	if _, err := s.Commit(context.Background(), b); err == nil {
		t.Fatal("expected the save error")
	}
	state.err = nil
	n, err := s.Commit(context.Background(), b)
	if err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	if pub.calls != 1 {
		t.Errorf("published %d times, want 1", pub.calls)
	}

	// The next batch is published again.
	if _, err := s.Commit(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if pub.calls != 2 {
		t.Errorf("published %d times, want 2", pub.calls)
	}
}

// TestPublishAgainstBroker needs a real broker, for example:
//
//	MAILSTATUS_TEST_RABBITMQ_URL=amqp://guest:guest@localhost:5672/ go test ./internal/rabbitmq
func TestPublishAgainstBroker(t *testing.T) {
	url := os.Getenv("MAILSTATUS_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("MAILSTATUS_TEST_RABBITMQ_URL not set")
	}
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	q, err := ch.QueueDeclare("", false, true, true, false, nil) // server-named, temporary
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, err := Open(Options{URL: url, RoutingKey: q.Name, Type: "mail_status", HeaderType: "fuglu", AppID: "test", Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()
	if err := p.Publish(ctx, []mailstatus.Event{event("u1", "a@x.com", mailstatus.Success, "ok", 0)}); err != nil {
		t.Fatal(err)
	}
	var d amqp.Delivery
	for range 50 {
		var ok bool
		if d, ok, err = ch.Get(q.Name, true); err != nil {
			t.Fatal(err)
		} else if ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var got LogDetails
	if err := json.Unmarshal(d.Body, &got); err != nil {
		t.Fatalf("body %q: %v", d.Body, err)
	}
	if got.EUID != "YSPL-1" || d.DeliveryMode != amqp.Persistent || d.MessageId != "YSPL-1" ||
		d.ContentType != "application/json" || d.Exchange != "" || d.Timestamp.IsZero() ||
		d.Headers["type"] != "fuglu" {
		t.Errorf("delivery = %+v, details = %+v", d, got)
	}

	// No queue for this routing key: an error for the collector to retry.
	bad, err := Open(Options{URL: url, RoutingKey: q.Name + ".missing", Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.Publish(ctx, []mailstatus.Event{event("u2", "a@x.com", mailstatus.Success, "ok", 0)}); err == nil {
		t.Error("an unroutable message must be an error")
	}
}

func TestGroupKeepsEachRecipientsMXResponse(t *testing.T) {
	a := event("u1", "a@gmail.com", mailstatus.Success, "ok", 0)
	a.Relay, a.DSN, a.Response = "gmail-smtp-in.l.google.com[74.125.131.27]:25", "2.0.0", "250 2.0.0 OK - gsmtp"
	b := event("u2", "b@outlook.com", mailstatus.Success, "ok", time.Second)
	b.Relay, b.DSN, b.Response = "outlook-com.olc.protection.outlook.com[52.101.103.161]:25", "2.6.0", "250 2.6.0 Queued mail for delivery"
	msgs := Group([]mailstatus.Event{a, b}, "t", nil)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	got := msgs[0].Details.MetaData["mx_responses"].([]MXResponse)
	want := []MXResponse{
		{EventUID: "u1", To: "a@gmail.com", Relay: a.Relay, DSN: "2.0.0", Response: a.Response},
		{EventUID: "u2", To: "b@outlook.com", Relay: b.Relay, DSN: "2.6.0", Response: b.Response},
	}
	if !slices.Equal(got, want) {
		t.Errorf("mx_responses = %+v", got)
	}
	body, _ := json.Marshal(msgs[0].Details)
	if !json.Valid(body) || !strings.Contains(string(body), `"mx_responses":[{"event_uid":"u1","to":"a@gmail.com","relay":"gmail-smtp-in`) {
		t.Errorf("json = %s", body)
	}
}
