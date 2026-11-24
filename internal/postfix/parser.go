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

// Package postfix parses the Postfix log lines that describe a message's life
// in the queue: cleanup (reference header), qmgr (sender, expiry, removal) and
// the delivery agents (one result per recipient per attempt).
package postfix

import (
	"mime"
	"regexp"
	"strings"
)

// Kind identifies which queue event a log line describes.
type Kind int

// Kinds of queue events. KindUnknown is a line the collector does not use.
const (
	KindUnknown   Kind = iota
	KindHeader         // cleanup: header_checks WARN/INFO hit on the reference header
	KindQueued         // qmgr: from=<...>, size=..., nrcpt=... (queue active)
	KindDelivery       // smtp/lmtp/local/...: to=<...>, ..., status=...
	KindExpired        // qmgr: from=<...>, status=expired, returned to sender
	KindRemoved        // qmgr: removed
	KindMessageID      // cleanup: message-id=<...>
	KindSubject        // cleanup: header_checks WARN/INFO hit on the Subject header
)

// String returns a short lower-case name for k, for logs and tests.
func (k Kind) String() string {
	switch k {
	case KindHeader:
		return "header"
	case KindQueued:
		return "queued"
	case KindDelivery:
		return "delivery"
	case KindExpired:
		return "expired"
	case KindRemoved:
		return "removed"
	case KindMessageID:
		return "message-id"
	case KindSubject:
		return "subject"
	default:
		return "unknown"
	}
}

// Delivery statuses logged by the delivery agents.
const (
	StatusSent     = "sent"
	StatusDeferred = "deferred"
	StatusBounced  = "bounced"
)

// Record is one parsed log line. Only the fields relevant to Kind are set.
type Record struct {
	Kind    Kind
	QueueID string

	From    string // KindQueued, KindExpired, KindHeader (when logged)
	HasFrom bool   // From was present on the line ("" is the null sender <>)

	HeaderValue string // KindHeader; KindSubject (decoded)
	MessageID   string // KindMessageID, without <>; "" when the mail has none

	To     string // KindDelivery
	OrigTo string // KindDelivery, set when Postfix rewrote the recipient
	Relay  string // KindDelivery
	DSN    string // KindDelivery, e.g. "5.1.1"
	Status string // KindDelivery: sent, deferred, bounced, ...
	Text   string // KindDelivery: the text in parentheses after status=
}

// Recipient is the address the sender used: orig_to when Postfix rewrote it
// (aliases, virtual), else to.
func (r Record) Recipient() string {
	if r.OrigTo != "" {
		return r.OrigTo
	}
	return r.To
}

// The parser uses plain string operations rather than regular expressions:
// it runs on every Postfix log line, and this is several times faster.

var fromRe = regexp.MustCompile(`(?:^|\s)from=<([^>]*)>`) // header lines only (rare)

// Words Postfix puts where a queue ID would be; they are never queue IDs.
var notQueueIDs = map[string]bool{
	"NOQUEUE": true, "warning": true, "error": true, "fatal": true,
	"panic": true, "statistics": true,
}

// Parser parses Postfix log messages. It is safe for concurrent use.
type Parser struct {
	header string // reference header name, compared case-insensitively
}

// NewParser returns a parser that recognises header_checks log lines for
// referenceHeader (matched case-insensitively, like Postfix header names).
func NewParser(referenceHeader string) *Parser {
	return &Parser{header: referenceHeader}
}

// Parse parses the MESSAGE of one Postfix journal entry. Lines that do not
// describe a queue event return a Record with Kind == KindUnknown.
func (p *Parser) Parse(msg string) Record {
	qid, rest, ok := splitQueueID(msg)
	if !ok {
		return Record{}
	}

	switch {
	case rest == "removed":
		return Record{Kind: KindRemoved, QueueID: qid}

	case strings.HasPrefix(rest, "to=<"):
		return parseDelivery(qid, rest)

	case strings.HasPrefix(rest, "message-id="):
		// Logged by cleanup for every mail. With always_add_missing_headers
		// Postfix adds one when the client sent none; otherwise it is "<>".
		return Record{Kind: KindMessageID, QueueID: qid, MessageID: trimAngles(strings.TrimSpace(rest[len("message-id="):]))}

	case strings.HasPrefix(rest, "from=<"):
		from, after, ok := cutAngles(rest[len("from="):])
		if !ok {
			return Record{}
		}
		switch {
		case strings.HasPrefix(after, ", status=expired"):
			return Record{Kind: KindExpired, QueueID: qid, From: from, HasFrom: true}
		case strings.HasPrefix(after, ", size=") && strings.Contains(after, ", nrcpt="):
			return Record{Kind: KindQueued, QueueID: qid, From: from, HasFrom: true}
		}

	default:
		if value, ok := headerValue(rest, p.header); ok {
			return parseHeader(qid, value)
		}
		if value, ok := headerValue(rest, "Subject"); ok {
			r := parseHeader(qid, value)
			r.Kind, r.HeaderValue = KindSubject, decodeSubject(r.HeaderValue)
			return r
		}
	}
	return Record{}
}

// splitQueueID splits "<queue id>: <rest>". Short IDs are hex, long IDs
// (enable_long_queue_ids) are base-52-ish; both are plain alphanumerics.
func splitQueueID(msg string) (qid, rest string, ok bool) {
	i := 0
	for i < len(msg) && isAlnum(msg[i]) {
		i++
	}
	if i < 5 || len(msg) < i+2 || msg[i] != ':' || msg[i+1] != ' ' {
		return "", "", false
	}
	if notQueueIDs[msg[:i]] {
		return "", "", false
	}
	return msg[:i], msg[i+2:], true
}

// headerValue matches "warning: header <name>: <value>" (or "info: ...")
// and returns <value>. Names are compared case-insensitively.
func headerValue(rest, name string) (string, bool) {
	s, ok := strings.CutPrefix(rest, "warning: header ")
	if !ok {
		if s, ok = strings.CutPrefix(rest, "info: header "); !ok {
			return "", false
		}
	}
	n := len(name)
	if len(s) <= n || s[n] != ':' || !strings.EqualFold(s[:n], name) {
		return "", false
	}
	return strings.TrimLeft(s[n+1:], " \t"), true
}

// maxLoggedSubject is how much of a Subject value Postfix logs: it cuts
// "Subject: <value>" at 200 characters.
const maxLoggedSubject = 200 - len("Subject: ")

// incompleteWordRe matches an RFC 2047 encoded word cut off at the end of s.
var incompleteWordRe = regexp.MustCompile(`=\?[^?\s]*(\?[bBqQ]?(\?[^?\s]*\??)?)?$`)

// decodeSubject turns the logged Subject into readable text. Mail clients send
// non-ASCII subjects as "=?UTF-8?B?...?=", and Postfix logs them as they are.
// A subject that cannot be decoded (e.g. an unknown charset) is returned as logged.
func decodeSubject(s string) string {
	// Postfix logs the line break of a folded header as "?".
	s = strings.ReplaceAll(s, "?=? ", "?= ")
	// At the length limit the last encoded word may be cut off and undecodable.
	if len(s) >= maxLoggedSubject {
		s = strings.TrimRight(incompleteWordRe.ReplaceAllString(s, ""), " ")
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(s)
	if err != nil {
		return s
	}
	return decoded
}

// cutAngles parses "<addr>rest" and returns addr and rest.
func cutAngles(s string) (addr, rest string, ok bool) {
	if !strings.HasPrefix(s, "<") {
		return "", "", false
	}
	addr, rest, ok = strings.Cut(s[1:], ">")
	return addr, rest, ok
}

// isAlnum reports whether c is an ASCII letter or digit.
func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// parseHeader parses the part after "header Name: " of a header_checks log line:
//
//	<value> from <client>; from=<sender> to=<rcpt> proto=ESMTP helo=<name>
//
// Postfix logs at most ~200 characters of the header, which is plenty for an ID.
func parseHeader(qid, s string) Record {
	r := Record{Kind: KindHeader, QueueID: qid}
	value := s
	if i := strings.LastIndex(s, "; from=<"); i >= 0 {
		value = s[:i]
		if m := fromRe.FindStringSubmatch(s[i+1:]); m != nil {
			r.From, r.HasFrom = m[1], true
		}
		// Drop the trailing " from <client>" (e.g. "localhost[127.0.0.1]", "local").
		// The added space covers an empty header, which leaves only "from <client>".
		value = " " + value
		if j := strings.LastIndex(value, " from "); j >= 0 {
			value = value[:j]
		}
	}
	r.HeaderValue = strings.TrimSpace(value)
	return r
}

// parseDelivery parses:
//
//	to=<a@b>, orig_to=<c@d>, relay=mx[1.2.3.4]:25, delay=1.2, delays=0/0/0.5/0.7, dsn=2.0.0, status=sent (250 2.0.0 OK)
func parseDelivery(qid, s string) Record {
	i := strings.Index(s, ", status=")
	if i < 0 {
		return Record{}
	}
	r := Record{Kind: KindDelivery, QueueID: qid}

	for _, kv := range strings.Split(s[:i], ", ") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "to":
			r.To = trimAngles(v)
		case "orig_to":
			r.OrigTo = trimAngles(v)
		case "relay":
			r.Relay = v
		case "dsn":
			r.DSN = v
		}
	}

	status := s[i+len(", status="):]
	if word, text, ok := strings.Cut(status, " ("); ok {
		r.Status = word
		r.Text = strings.TrimSuffix(text, ")")
	} else {
		r.Status = strings.TrimSpace(status)
	}
	if r.To == "" || r.Status == "" {
		return Record{}
	}
	return r
}

// trimAngles removes one leading "<" and one trailing ">" from s.
func trimAngles(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">")
}
