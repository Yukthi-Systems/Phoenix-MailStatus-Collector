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

// Package config loads the collector's settings from environment variables,
// which is what a systemd EnvironmentFile provides.
//
// Every variable is prefixed MAILSTATUS_. Load collects all problems it finds
// and returns them together, so a bad configuration is fixed in one pass.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration.
type Config struct {
	// RabbitMQURL is the AMQP URL of the broker. Required except in dry-run mode.
	RabbitMQURL        string
	RabbitMQExchange   string // "" is the default exchange: the routing key is then the queue name
	RabbitMQRoutingKey string
	LogType            string         // LogDetails.Type
	TimeZone           *time.Location // zone of the message timestamps
	RabbitMQHeaderType string         // AMQP header "type", which the log consumer routes on

	// StateFile holds the journal position and in-flight messages.
	// Required for following the journal.
	StateFile string

	// Node names this mail server. It is published as meta_data.server and is
	// part of every event ID, so it must be unique per server and must never
	// change for a server.
	Node string

	ReferenceHeader string // header logged by header_checks
	SuccessCode     string // error_code reported for delivered mail

	// InternalRelays are next hops that re-queue mail on this Postfix (e.g. a
	// DKIM-signing filter). Reaching them is not reported as delivery.
	InternalRelays []string

	Journalctl  string   // journalctl binary
	Identifiers []string // SYSLOG_IDENTIFIERs to follow

	BatchSize     int
	FlushInterval time.Duration
	StatsInterval time.Duration
	StateMaxAge   time.Duration // forget queue IDs not seen for this long

	LogLevel  slog.Level
	LogFormat string // "json" or "text" (logfmt)

	// LogFile is the log file path. Empty: log to stderr (the journal under systemd).
	LogFile     string
	LogMaxSize  int64         // bytes before rotation
	LogMaxFiles int           // rotated files kept
	LogMaxAge   time.Duration // rotated files older than this are removed
	LogCompress bool          // gzip rotated files
}

// Postfix services whose log lines matter. cleanup: reference header;
// qmgr: sender, expiry, removal; the rest: delivery results.
var defaultServices = []string{"cleanup", "qmgr", "smtp", "lmtp", "local", "virtual", "pipe", "error", "discard"}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	var errs []error
	hostname, _ := os.Hostname()
	c := Config{
		RabbitMQURL:        str("MAILSTATUS_RABBITMQ_URL", ""),
		RabbitMQExchange:   str("MAILSTATUS_RABBITMQ_EXCHANGE", ""),
		RabbitMQRoutingKey: str("MAILSTATUS_RABBITMQ_ROUTING_KEY", ""),
		LogType:            str("MAILSTATUS_LOG_TYPE", "Outgoing"),
		RabbitMQHeaderType: str("MAILSTATUS_RABBITMQ_HEADER_TYPE", "fuglu"),
		StateFile:          str("MAILSTATUS_STATE_FILE", ""),
		Node:               str("MAILSTATUS_NODE", hostname),
		ReferenceHeader:    str("MAILSTATUS_REFERENCE_HEADER", "X-YSPL-ID"),
		SuccessCode:        str("MAILSTATUS_SUCCESS_CODE", "255"),
		Journalctl:         str("MAILSTATUS_JOURNALCTL", "journalctl"),
		BatchSize:          integer("MAILSTATUS_BATCH_SIZE", 500, &errs),
		FlushInterval:      duration("MAILSTATUS_FLUSH_INTERVAL", time.Second, &errs),
		StatsInterval:      duration("MAILSTATUS_STATS_INTERVAL", 5*time.Minute, &errs),
		StateMaxAge:        duration("MAILSTATUS_STATE_MAX_AGE", 30*24*time.Hour, &errs),
		LogFormat:          strings.ToLower(str("MAILSTATUS_LOG_FORMAT", "json")),
		LogFile:            os.Getenv("MAILSTATUS_LOG_FILE"),
		LogMaxSize:         int64(integer("MAILSTATUS_LOG_MAX_SIZE_MB", 100, &errs)) << 20,
		LogMaxFiles:        integer("MAILSTATUS_LOG_MAX_FILES", 10, &errs),
		LogMaxAge:          duration("MAILSTATUS_LOG_MAX_AGE", 30*24*time.Hour, &errs),
		LogCompress:        boolean("MAILSTATUS_LOG_COMPRESS", true, &errs),
	}
	tz := str("MAILSTATUS_TIME_ZONE", "Asia/Kolkata")
	if loc, err := time.LoadLocation(tz); err != nil {
		errs = append(errs, fmt.Errorf("MAILSTATUS_TIME_ZONE: %w", err))
	} else {
		c.TimeZone = loc
	}
	c.InternalRelays = strings.FieldsFunc(os.Getenv("MAILSTATUS_INTERNAL_RELAYS"), func(r rune) bool { return r == ',' || r == ' ' })
	if c.RabbitMQURL != "" && c.RabbitMQRoutingKey == "" {
		errs = append(errs, errors.New("MAILSTATUS_RABBITMQ_ROUTING_KEY: required with MAILSTATUS_RABBITMQ_URL (the queue name when using the default exchange)"))
	}

	// Postfix's syslog_name (multi-instance setups use e.g. "postfix-out").
	syslogName := str("MAILSTATUS_POSTFIX_SYSLOG_NAME", "postfix")
	services := defaultServices
	if v := os.Getenv("MAILSTATUS_POSTFIX_SERVICES"); v != "" {
		services = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	for _, s := range services {
		c.Identifiers = append(c.Identifiers, syslogName+"/"+s)
	}

	if err := c.LogLevel.UnmarshalText([]byte(str("MAILSTATUS_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("MAILSTATUS_LOG_LEVEL: %w", err))
	}

	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("MAILSTATUS_LOG_FORMAT: %q is not json or text", c.LogFormat))
	}
	if c.LogMaxSize < 1<<20 {
		errs = append(errs, errors.New("MAILSTATUS_LOG_MAX_SIZE_MB: must be at least 1"))
	}
	if c.LogMaxFiles < 0 || c.LogMaxAge < 0 {
		errs = append(errs, errors.New("MAILSTATUS_LOG_MAX_FILES / MAILSTATUS_LOG_MAX_AGE: must not be negative"))
	}

	if c.Node == "" {
		errs = append(errs, errors.New("MAILSTATUS_NODE: not set and the hostname is unknown"))
	}
	if c.ReferenceHeader == "" || strings.ContainsAny(c.ReferenceHeader, ": \t") {
		errs = append(errs, fmt.Errorf("MAILSTATUS_REFERENCE_HEADER: invalid header name %q", c.ReferenceHeader))
	}
	if len(c.SuccessCode) > 16 {
		errs = append(errs, errors.New("MAILSTATUS_SUCCESS_CODE: longer than 16 characters"))
	}
	if c.BatchSize < 1 {
		errs = append(errs, errors.New("MAILSTATUS_BATCH_SIZE: must be at least 1"))
	}
	if c.FlushInterval <= 0 {
		errs = append(errs, errors.New("MAILSTATUS_FLUSH_INTERVAL: must be positive"))
	}
	return c, errors.Join(errs...)
}

// str returns the trimmed value of key, or def when it is unset or blank.
func str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// integer returns key parsed as an int, or def when unset. A parse error is
// appended to errs and def is returned.
func integer(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

// boolean returns key parsed by strconv.ParseBool, or def when unset. A parse
// error is appended to errs and def is returned.
func boolean(key string, def bool, errs *[]error) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

// duration returns key parsed by time.ParseDuration (e.g. "1s", "720h"), or
// def when unset. A parse error is appended to errs and def is returned.
func duration(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}
