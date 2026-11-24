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

// Package tracker follows each message through the Postfix queue and turns
// parsed log lines into mailstatus.Event values.
//
// Postfix logs the sender, the reference header and each delivery result on
// separate lines, tied together only by the queue ID. The tracker keeps that
// per-message context until qmgr logs "removed". Its state is persisted with
// every batch (see Snapshot / Restore), so a restart continues mid-message.
package tracker

import (
	"encoding/json"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/postfix"
)

// Defaults used when no SMTP reply code can be found in the log text
// (connection timeouts, DNS failures, ...).
const (
	DefaultDeferredCode = "451"
	DefaultFailureCode  = "554"
	successRemarks      = "Mail delivered successfully"
)

// Message is the context kept for one queue ID.
type Message struct {
	From        string               `json:"from"`
	FromKnown   bool                 `json:"from_known"` // From was logged ("" then means the null sender)
	ReferenceID string               `json:"reference_id,omitempty"`
	MessageID   string               `json:"message_id,omitempty"`
	Subject     string               `json:"subject,omitempty"`
	Pending     map[string]*Deferral `json:"pending,omitempty"` // recipients whose last attempt was deferred
	FirstSeen   time.Time            `json:"first_seen,omitzero"`
	LastSeen    time.Time            `json:"last_seen"`
}

// Deferral is the last temporary failure of a recipient. It becomes the
// reason of the Failure event if the message expires.
type Deferral struct {
	Reason string `json:"reason"`
	Code   string `json:"code"`
	Relay  string `json:"relay,omitempty"`
	DSN    string `json:"dsn,omitempty"`
}

// isNotification reports whether this is a message with the null sender
// (bounces, delay warnings). Those are Postfix's own mail and are not recorded.
func (m *Message) isNotification() bool { return m.FromKnown && m.From == "" }

// Config holds the tracker settings.
type Config struct {
	Node        string        // this mail server; reported as Event.Server, part of Event.UID
	SuccessCode string        // error_code for Success events
	MaxAge      time.Duration // forget messages not seen for this long

	// InternalRelays are next hops that hand the mail back to this Postfix under
	// a new queue ID (e.g. a DKIM-signing content filter), by host name.
	InternalRelays []string
}

// Tracker holds the context of every message still in the Postfix queue.
// It is not safe for concurrent use; the collector drives it from one goroutine.
type Tracker struct {
	cfg      Config
	log      *slog.Logger
	messages map[string]*Message
	dirty    map[string]bool // queue IDs changed since the last Snapshot
}

// New returns an empty tracker.
func New(cfg Config, log *slog.Logger) *Tracker {
	return &Tracker{
		cfg:      cfg,
		log:      log,
		messages: make(map[string]*Message),
		dirty:    make(map[string]bool),
	}
}

// Len returns the number of messages being tracked.
func (t *Tracker) Len() int { return len(t.messages) }

// Handle applies one parsed log line and returns the events it produces.
func (t *Tracker) Handle(e journal.Entry, r postfix.Record) []mailstatus.Event {
	switch r.Kind {
	case postfix.KindHeader:
		m := t.get(r.QueueID, e.Time)
		if m.ReferenceID == "" { // first occurrence wins
			m.ReferenceID = r.HeaderValue
		}
		if r.HasFrom && !m.FromKnown {
			m.From, m.FromKnown = r.From, true
		}

	case postfix.KindSubject:
		m := t.get(r.QueueID, e.Time)
		if m.Subject == "" { // first occurrence wins
			m.Subject = r.HeaderValue
		}
		if r.HasFrom && !m.FromKnown {
			m.From, m.FromKnown = r.From, true
		}

	case postfix.KindMessageID:
		m := t.get(r.QueueID, e.Time)
		if m.MessageID == "" {
			m.MessageID = r.MessageID
		}

	case postfix.KindQueued:
		m := t.get(r.QueueID, e.Time)
		m.From, m.FromKnown = r.From, true

	case postfix.KindDelivery:
		return t.delivery(e, r)

	case postfix.KindExpired:
		return t.expired(e, r)

	case postfix.KindRemoved:
		if m, ok := t.messages[r.QueueID]; ok {
			if len(m.Pending) > 0 && !m.isNotification() {
				t.log.Warn("message removed with deferred recipients and no final status",
					"queue_id", r.QueueID, "recipients", slices.Sorted(maps.Keys(m.Pending)))
			}
			t.forget(r.QueueID)
		}
	}
	return nil
}

// delivery turns one delivery-agent result into an event, updating the
// message's pending recipients. Results for notifications, internal-relay
// hand-offs and address probes produce no event.
func (t *Tracker) delivery(e journal.Entry, r postfix.Record) []mailstatus.Event {
	m := t.get(r.QueueID, e.Time)
	if m.isNotification() {
		return nil
	}
	// Reaching an internal relay is not delivery: the result that counts comes
	// from the next queue ID. Deferred and bounced stay reported, because that
	// mail has not gone out.
	if r.Status == postfix.StatusSent && t.isInternalRelay(r.Relay) {
		t.handOver(m, r, e.Time)
		t.settle(r.QueueID, m, r.Recipient())
		return nil
	}
	if !m.FromKnown {
		// Started mid-message (first run, or the qmgr line was before the
		// saved position). The row is still recorded, without a sender.
		t.log.Warn("delivery for a message whose sender was not seen", "queue_id", r.QueueID)
	}

	rcpt := r.Recipient()
	ev := mailstatus.Event{
		UID:         mailstatus.NewUID(t.cfg.Node, e.Cursor, rcpt),
		From:        m.From,
		To:          rcpt,
		Time:        e.Time,
		ReferenceID: m.ReferenceID,
		MessageID:   m.MessageID,
		QueueID:     r.QueueID,
		Server:      t.cfg.Node,
		Subject:     m.Subject,
		QueuedAt:    m.FirstSeen,
		Relay:       r.Relay,
		DSN:         r.DSN,
		Response:    r.Text,
	}

	switch r.Status {
	case postfix.StatusSent:
		ev.Status, ev.Remarks, ev.ErrorCode = mailstatus.Success, successRemarks, t.cfg.SuccessCode
		t.settle(r.QueueID, m, rcpt)
	case postfix.StatusDeferred:
		ev.Status, ev.Remarks, ev.ErrorCode = mailstatus.Deferred, r.Text, ReplyCode(r.Text, DefaultDeferredCode)
		if m.Pending == nil {
			m.Pending = make(map[string]*Deferral)
		}
		m.Pending[rcpt] = &Deferral{Reason: r.Text, Code: ev.ErrorCode, Relay: r.Relay, DSN: r.DSN}
	case postfix.StatusBounced:
		ev.Status, ev.Remarks, ev.ErrorCode = mailstatus.Failure, r.Text, ReplyCode(r.Text, DefaultFailureCode)
		t.settle(r.QueueID, m, rcpt)
	default:
		// "deliverable"/"undeliverable" (address verification probes) and
		// other non-final states are not delivery outcomes.
		return nil
	}
	return []mailstatus.Event{ev}
}

// isInternalRelay matches the host in a relay field: "host[1.2.3.4]:25".
func (t *Tracker) isInternalRelay(relay string) bool {
	host, _, _ := strings.Cut(relay, "[")
	return slices.ContainsFunc(t.cfg.InternalRelays, func(r string) bool { return strings.EqualFold(r, host) })
}

// handOver copies a message's context to the queue ID the internal relay
// re-queued it under ("... queued as <id>"). The re-queued mail logs no header
// lines of its own, so its deliveries would otherwise have no subject or reference.
func (t *Tracker) handOver(from *Message, r postfix.Record, now time.Time) {
	match := queuedAsRe.FindStringSubmatch(r.Text)
	if match == nil {
		t.log.Warn("internal relay accepted a message without re-queuing it; no status will follow",
			"queue_id", r.QueueID, "recipient", r.Recipient(), "relay", r.Relay, "reply", r.Text)
		return
	}
	next := t.get(match[1], now)
	if next.ReferenceID == "" {
		next.ReferenceID = from.ReferenceID
	}
	if next.Subject == "" {
		next.Subject = from.Subject
	}
	if next.MessageID == "" {
		next.MessageID = from.MessageID
	}
	if !next.FromKnown {
		next.From, next.FromKnown = from.From, from.FromKnown
	}
	if !from.FirstSeen.IsZero() && from.FirstSeen.Before(next.FirstSeen) {
		next.FirstSeen = from.FirstSeen
	}
}

// expired turns every still-deferred recipient into a Failure. qmgr logs the
// expiry once per message, not per recipient, so the tracker has to know who
// was still pending.
func (t *Tracker) expired(e journal.Entry, r postfix.Record) []mailstatus.Event {
	m := t.get(r.QueueID, e.Time)
	if !m.FromKnown {
		m.From, m.FromKnown = r.From, true
	}
	if m.isNotification() || len(m.Pending) == 0 {
		return nil
	}
	events := make([]mailstatus.Event, 0, len(m.Pending))
	for _, rcpt := range slices.Sorted(maps.Keys(m.Pending)) {
		d := m.Pending[rcpt]
		code := d.Code
		if code == "" {
			code = DefaultFailureCode
		}
		events = append(events, mailstatus.Event{
			UID:         mailstatus.NewUID(t.cfg.Node, e.Cursor, rcpt),
			From:        m.From,
			To:          rcpt,
			Time:        e.Time,
			ReferenceID: m.ReferenceID,
			MessageID:   m.MessageID,
			Status:      mailstatus.Failure,
			Expired:     true,
			Remarks:     "Delivery time expired, gave up: " + d.Reason,
			ErrorCode:   code,
			QueueID:     r.QueueID,
			Server:      t.cfg.Node,
			Subject:     m.Subject,
			QueuedAt:    m.FirstSeen,
			Relay:       d.Relay,
			DSN:         d.DSN,
			Response:    d.Reason,
		})
	}
	m.Pending = nil
	t.dirty[r.QueueID] = true
	return events
}

// get returns the message for qid, creating it first-seen at now, and marks
// it changed.
func (t *Tracker) get(qid string, now time.Time) *Message {
	m, ok := t.messages[qid]
	if !ok {
		m = &Message{FirstSeen: now}
		t.messages[qid] = m
	}
	if now.After(m.LastSeen) {
		m.LastSeen = now
	}
	t.dirty[qid] = true
	return m
}

// settle drops rcpt from the message's deferred recipients: it reached a
// final state.
func (t *Tracker) settle(qid string, m *Message, rcpt string) {
	if _, ok := m.Pending[rcpt]; ok {
		delete(m.Pending, rcpt)
		t.dirty[qid] = true
	}
}

// forget stops tracking qid; the next Snapshot reports it as deleted.
func (t *Tracker) forget(qid string) {
	delete(t.messages, qid)
	t.dirty[qid] = true
}

// Expire forgets messages not seen for MaxAge before now (the time of the
// newest log line, so replaying old logs does not expire live messages).
// Normally "removed" cleans up; this covers lines lost to journal rate limits.
func (t *Tracker) Expire(now time.Time) int {
	if t.cfg.MaxAge <= 0 {
		return 0
	}
	cutoff := now.Add(-t.cfg.MaxAge)
	n := 0
	for qid, m := range t.messages {
		if m.LastSeen.Before(cutoff) {
			t.forget(qid)
			n++
		}
	}
	return n
}

// State is the persisted form of one message.
type State struct {
	QueueID  string
	JSON     []byte
	LastSeen time.Time
}

// Snapshot returns the changes since the last Committed call: messages to
// upsert and queue IDs to delete. The data is copied. Calling it again before
// Committed returns the same changes plus any newer ones, so a failed write
// can simply be retried with a fresh snapshot.
func (t *Tracker) Snapshot() (upserts []State, deletes []string) {
	for qid := range t.dirty {
		m, ok := t.messages[qid]
		if !ok {
			deletes = append(deletes, qid)
			continue
		}
		b, err := json.Marshal(m)
		if err != nil { // cannot happen for this type
			panic(err)
		}
		upserts = append(upserts, State{QueueID: qid, JSON: b, LastSeen: m.LastSeen})
	}
	return upserts, deletes
}

// Committed marks the last Snapshot as durably written.
func (t *Tracker) Committed() { clear(t.dirty) }

// Restore loads persisted messages, replacing the current state.
func (t *Tracker) Restore(states []State) error {
	clear(t.messages)
	clear(t.dirty)
	for _, s := range states {
		var m Message
		if err := json.Unmarshal(s.JSON, &m); err != nil {
			return err
		}
		t.messages[s.QueueID] = &m
	}
	return nil
}

var (
	// "host mx[1.2.3.4] said: 550 5.1.1 ..." - the remote server's reply.
	saidCodeRe = regexp.MustCompile(`said: ([245]\d\d)[ -]`)
	// "250 2.0.0 Ok: queued as ..." - status text that is itself a reply.
	leadCodeRe = regexp.MustCompile(`^([245]\d\d)[ -]`)
	// "250 FUGLU REQUEUE(...): 2.0.0 Ok: queued as 8FA5B3EAE6"
	queuedAsRe = regexp.MustCompile(`queued as ([0-9A-Za-z]+)`)
)

// ReplyCode returns the SMTP reply code in a Postfix status text, or def.
// It only looks at reply positions, never at arbitrary numbers, so IP octets
// like "203" in "mx[203.0.113.9]" are not mistaken for codes.
func ReplyCode(text, def string) string {
	if m := saidCodeRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	if m := leadCodeRe.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		return m[1]
	}
	return def
}
