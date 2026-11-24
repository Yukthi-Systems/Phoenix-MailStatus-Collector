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

// Package rabbitmq publishes delivery results to RabbitMQ as LogDetails
// messages, using publisher confirms so that a batch only counts as written
// once the broker has accepted every message in it.
package rabbitmq

import (
	"slices"
	"strings"
	"time"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
)

// Values of LogDetails.Status. Deferred can be followed by another status for
// the same recipient; the others are final.
const (
	StatusDelivered = "Delivered" // the receiving server accepted the mail
	StatusDeferred  = "Deferred"  // temporary failure, Postfix will retry
	StatusBounced   = "Bounced"   // permanently rejected
	StatusExpired   = "Expired"   // still failing when the queue lifetime ran out
)

// messageStatus returns the LogDetails.Status for an event.
func messageStatus(e mailstatus.Event) string {
	switch {
	case e.Expired:
		return StatusExpired
	case e.Status == mailstatus.Success:
		return StatusDelivered
	case e.Status == mailstatus.Failure:
		return StatusBounced
	default:
		return StatusDeferred
	}
}

// LogDetails is the JSON message body: one mail, one outcome, and the
// recipients that share it.
type LogDetails struct {
	EUID              string         `json:"euid"`
	MessageID         string         `json:"message_id"`
	FromEmailId       string         `json:"from_email_id"`
	ToEmailIds        []string       `json:"to_email_ids"`
	EmailDomains      []string       `json:"email_domains"`
	Type              string         `json:"type"`
	Status            string         `json:"status"`
	StatusDescription string         `json:"status_description"`
	Subject           string         `json:"subject"`
	LogTimestamp      string         `json:"log_timestamp"`
	EmailTimeStamp    string         `json:"email_timestamp"`
	MetaData          map[string]any `json:"meta_data,omitempty"`
}

// MXResponse is the reply logged for one recipient: what the receiving mail
// server answered, or Postfix's reason when it could not reach one.
// meta_data.mx_responses holds one per event, in log order.
type MXResponse struct {
	EventUID string `json:"event_uid"`
	To       string `json:"to"`
	Relay    string `json:"relay"`    // "host[ip]:port"; "" or "none" when no server was reached
	DSN      string `json:"dsn"`      // enhanced status code, e.g. "2.0.0", "5.1.1"
	Response string `json:"response"` // e.g. "250 2.0.0 OK  1794559714 ... - gsmtp"
}

// Message is one AMQP message.
type Message struct {
	ID      string // stable ID of the merged events, the same when a batch is retried
	Details LogDetails
}

// Group merges per-recipient events into messages.
//
// Postfix logs one line per recipient, but LogDetails has one status for a list
// of recipients. So events are merged only when they belong to the same mail
// (queue ID) and have the same outcome. Recipients of one mail with different
// outcomes, or whose results arrive in different batches, get separate messages.
//
// meta_data.event_uids maps each event's stable ID to its recipient. Publishing
// is at-least-once and a replay can merge events differently, so consumers
// should de-duplicate on these IDs, not on the message.
//
// Timestamps are given in zone (nil: UTC), like Fuglu's records (+05:30).
func Group(events []mailstatus.Event, logType string, zone *time.Location) []Message {
	if zone == nil {
		zone = time.UTC
	}
	type key struct{ queueID, status, remarks, code string }
	index := make(map[key]int)
	var (
		out  []Message
		uids [][]string
	)
	for _, e := range events {
		k := key{e.QueueID, e.Status, e.Remarks, e.ErrorCode}
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, Message{Details: newDetails(e, logType, zone)})
			uids = append(uids, nil)
		}
		d := &out[i].Details
		if !slices.Contains(d.ToEmailIds, e.To) {
			d.ToEmailIds = append(d.ToEmailIds, e.To)
		}
		if domain := emailDomain(e.To); domain != "" && !slices.Contains(d.EmailDomains, domain) {
			d.EmailDomains = append(d.EmailDomains, domain)
		}
		if ts := formatTime(e.Time, zone); ts > d.LogTimestamp { // same format and zone, so this compares by time
			d.LogTimestamp = ts
		}
		d.MetaData["event_uids"].(map[string]string)[e.UID] = e.To
		d.MetaData["mx_responses"] = append(d.MetaData["mx_responses"].([]MXResponse), MXResponse{
			EventUID: e.UID, To: e.To, Relay: e.Relay, DSN: e.DSN, Response: e.Response,
		})
		uids[i] = append(uids[i], e.UID)
	}
	for i := range out {
		out[i].ID = mailstatus.NewUID(uids[i]...)
		// Like Fuglu: the recipients' domains and the sender's, sorted.
		d := &out[i].Details
		if domain := emailDomain(d.FromEmailId); domain != "" && !slices.Contains(d.EmailDomains, domain) {
			d.EmailDomains = append(d.EmailDomains, domain)
		}
		slices.Sort(d.EmailDomains)
	}
	return out
}

// noSubject is the subject reported for a mail that has none.
const noSubject = "No Subject"

// newDetails returns the LogDetails for the first event of a group, with
// empty recipient lists for Group to fill.
func newDetails(e mailstatus.Event, logType string, zone *time.Location) LogDetails {
	subject := e.Subject
	if subject == "" {
		subject = noSubject
	}
	queued := e.QueuedAt
	if queued.IsZero() {
		queued = e.Time // context lost (see README): the delivery time is the best guess
	}
	return LogDetails{
		EUID:              e.ReferenceID,
		MessageID:         e.MessageID,
		FromEmailId:       e.From,
		ToEmailIds:        []string{},
		EmailDomains:      []string{},
		Type:              logType,
		Status:            messageStatus(e),
		StatusDescription: e.Remarks,
		Subject:           subject,
		EmailTimeStamp:    formatTime(queued, zone),
		MetaData: map[string]any{
			"queue_id":     e.QueueID,
			"error_code":   e.ErrorCode,
			"server":       e.Server,
			"event_uids":   map[string]string{},
			"mx_responses": []MXResponse{},
		},
	}
}

// emailDomain returns the lower-cased domain of addr, or "" if it has no "@".
func emailDomain(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// timeLayout is RFC 3339 with a numeric offset ("+05:30"; "+00:00", never "Z",
// which Python's datetime.fromisoformat rejects before 3.11: the log consumer uses it).
const timeLayout = "2006-01-02T15:04:05-07:00"

// formatTime returns t in zone with second precision, or "" when t is unknown.
func formatTime(t time.Time, zone *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return t.In(zone).Format(timeLayout)
}
