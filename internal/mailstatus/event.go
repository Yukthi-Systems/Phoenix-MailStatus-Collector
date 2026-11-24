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

// Package mailstatus defines Event, the delivery result of one recipient, and
// the deterministic IDs that make re-published results recognisable.
package mailstatus

import (
	"crypto/sha1"
	"encoding/hex"
	"time"
)

// Values of Event.Status.
const (
	Success  = "Success"
	Deferred = "Deferred"
	Failure  = "Failure"
)

// Event is one delivery outcome for one recipient.
type Event struct {
	UID         string    `json:"event_uid"` // deterministic, see NewUID
	From        string    `json:"from_email"`
	To          string    `json:"to_email"`
	Time        time.Time `json:"event_time"`
	ReferenceID string    `json:"reference_id"`
	MessageID   string    `json:"message_id"` // Message-ID header, without <>
	Status      string    `json:"status"`
	Remarks     string    `json:"remarks"`
	ErrorCode   string    `json:"error_code"`
	QueueID     string    `json:"queue_id"`
	Server      string    `json:"server"`            // node that delivered the mail
	Expired     bool      `json:"expired,omitempty"` // Failure because the queue lifetime ran out
	Subject     string    `json:"subject,omitempty"`
	QueuedAt    time.Time `json:"queued_at,omitzero"` // when this server first logged the mail

	// What the receiving server (or Postfix, when it could not reach one) said.
	Relay    string `json:"relay,omitempty"`    // e.g. "gmail-smtp-in.l.google.com[74.125.131.27]:25"
	DSN      string `json:"dsn,omitempty"`      // enhanced status code, e.g. "2.0.0"
	Response string `json:"response,omitempty"` // text after status=, e.g. "250 2.0.0 OK ... - gsmtp"
}

// namespace is a fixed UUID used as the UUIDv5 namespace for event IDs.
var namespace = [16]byte{
	0x6f, 0x2b, 0x8c, 0x3e, 0x41, 0x0d, 0x4f, 0x6a,
	0x9b, 0x52, 0x1e, 0x7d, 0xc4, 0x30, 0x88, 0xa1,
}

// NewUID returns a name-based (version 5) UUID for parts.
//
// The collector derives an event's ID from the node name, the journal cursor
// of the log line and the recipient. Reading the same log line again - after a
// crash, a restart, or a backfill - produces the same ID, so a consumer can
// de-duplicate the at-least-once stream of results without a spool file.
func NewUID(parts ...string) string {
	h := sha1.New()
	h.Write(namespace[:])
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant

	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:], u[10:])
	return string(buf[:])
}
