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

// Command mailstatus-collector reads Postfix delivery results from the systemd
// journal and publishes them to RabbitMQ as JSON messages.
//
// Usage:
//
//	mailstatus-collector                       follow the journal (systemd service)
//	mailstatus-collector -stdin < export.json  re-publish a `journalctl -o json` export
//	mailstatus-collector -stdin -dry-run       print the messages instead of publishing them
//	mailstatus-collector -version              print the version and exit
//
// All settings come from environment variables; see the README.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // MAILSTATUS_TIME_ZONE works without zoneinfo files on the server

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/collector"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/config"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/logfile"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/postfix"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/rabbitmq"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/statefile"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

// main delegates to run so deferred cleanups execute before os.Exit.
func main() {
	os.Exit(run())
}

// run parses flags, loads the configuration, and runs the collector until
// SIGINT/SIGTERM or end of input. It returns the process exit code:
// 0 on a clean stop, 1 on a runtime error, 2 on invalid configuration.
func run() int {
	var (
		fromStdin   = flag.Bool("stdin", false, "read `journalctl -o json` output from stdin instead of following the journal (backfill)")
		dryRun      = flag.Bool("dry-run", false, "print the messages as JSON lines instead of publishing them")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("mailstatus-collector", version)
		return 0
	}

	cfg, err := config.Load()
	log, closeLog := newLogger(cfg)
	defer closeLog()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := collect(ctx, cfg, log, *fromStdin, *dryRun); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("stopped", "error", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

// collect builds the pipeline (journal reader -> parser -> tracker -> sink)
// and runs it. In live mode it restores the tracker and journal position from
// the state file first, so a restart continues where the last run stopped.
func collect(ctx context.Context, cfg config.Config, log *slog.Logger, fromStdin, dryRun bool) error {
	live := !fromStdin && !dryRun
	parser := postfix.NewParser(cfg.ReferenceHeader)
	trk := tracker.New(tracker.Config{
		Node:           cfg.Node,
		SuccessCode:    cfg.SuccessCode,
		MaxAge:         cfg.StateMaxAge,
		InternalRelays: cfg.InternalRelays,
	}, log)

	var (
		out    sink.Sink
		pos    journal.Position
		output []any // log attributes saying where results go
	)
	if dryRun {
		out = printSink{enc: json.NewEncoder(os.Stdout), logType: cfg.LogType, zone: cfg.TimeZone}
		output = []any{"output", "stdout"}
	} else {
		if cfg.RabbitMQURL == "" {
			return errors.New("MAILSTATUS_RABBITMQ_URL is required (use -dry-run to preview without a broker)")
		}
		rabbit := &rabbitmq.Sink{}
		if live {
			if cfg.StateFile == "" {
				return errors.New("MAILSTATUS_STATE_FILE must be set to follow the journal")
			}
			state, err := statefile.Open(cfg.StateFile)
			if err != nil {
				return err
			}
			var states []tracker.State
			pos, states = state.Load()
			if err := trk.Restore(states); err != nil {
				return fmt.Errorf("restore tracker state: %w", err)
			}
			log.Info("resuming", "cursor", pos.Cursor, "since", pos.Time, "tracked_messages", trk.Len())
			rabbit.State = state
		}
		pub, err := rabbitmq.Open(rabbitmq.Options{
			URL:        cfg.RabbitMQURL,
			Exchange:   cfg.RabbitMQExchange,
			RoutingKey: cfg.RabbitMQRoutingKey,
			Type:       cfg.LogType,
			TimeZone:   cfg.TimeZone,
			HeaderType: cfg.RabbitMQHeaderType,
			AppID:      "mailstatus-collector@" + cfg.Node,
			Log:        log,
		})
		if err != nil {
			return err
		}
		defer pub.Close()
		rabbit.Publisher = pub
		out = rabbit
		output = []any{"output", "rabbitmq", "rabbitmq_host", rabbitmq.Host(cfg.RabbitMQURL),
			"exchange", cfg.RabbitMQExchange, "routing_key", cfg.RabbitMQRoutingKey,
			"header_type", cfg.RabbitMQHeaderType}
	}

	reader := &journal.Reader{Journalctl: cfg.Journalctl, Identifiers: cfg.Identifiers, Log: log}
	entries := make(chan journal.Entry, 1024)
	readErr := make(chan error, 1)
	go func() {
		defer close(entries)
		if fromStdin {
			readErr <- reader.Stream(ctx, os.Stdin, entries)
		} else {
			readErr <- reader.Follow(ctx, pos, entries)
		}
	}()

	c := collector.New(collector.Options{
		BatchSize:     cfg.BatchSize,
		FlushInterval: cfg.FlushInterval,
		StatsInterval: cfg.StatsInterval,
		ExpireEvery:   time.Hour,
		Live:          live,
		ShutdownGrace: 10 * time.Second,
		Output:        output[1].(string),
	}, parser, trk, out, log)

	log.Info("started", append([]any{"version", version, "node", cfg.Node, "mode", mode(fromStdin, dryRun)},
		append(output, "identifiers", cfg.Identifiers, "reference_header", cfg.ReferenceHeader)...)...)

	if err := c.Run(ctx, entries); err != nil {
		return err
	}
	// Input ended. For stdin that is success; for the journal it means
	// journalctl died - exit non-zero so systemd restarts us from the
	// committed position.
	if err := <-readErr; err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if !fromStdin {
		return errors.New("journal reader stopped")
	}
	return nil
}

// mode names the run mode for the "started" log line.
func mode(fromStdin, dryRun bool) string {
	switch {
	case dryRun:
		return "dry-run"
	case fromStdin:
		return "backfill"
	default:
		return "live"
	}
}

// newLogger logs JSON or logfmt to MAILSTATUS_LOG_FILE (rotated), or to
// stderr when no file is set. With a file, warnings and errors also go to
// stderr so `systemctl status` shows them. Under systemd (JOURNAL_STREAM is
// set) the stderr timestamp is dropped because journald adds its own.
func newLogger(cfg config.Config) (*slog.Logger, func()) {
	stderr := newHandler(os.Stderr, cfg.LogFormat, cfg.LogLevel, os.Getenv("JOURNAL_STREAM") != "")
	if cfg.LogFile == "" {
		return slog.New(stderr), func() {}
	}
	f, err := logfile.Open(logfile.Options{
		Path:       cfg.LogFile,
		MaxSize:    cfg.LogMaxSize,
		MaxBackups: cfg.LogMaxFiles,
		MaxAge:     cfg.LogMaxAge,
		Compress:   cfg.LogCompress,
	})
	if err != nil {
		log := slog.New(stderr)
		log.Warn("cannot open log file, logging to stderr", "file", cfg.LogFile, "error", err)
		return log, func() {}
	}
	warnLevel := max(cfg.LogLevel, slog.LevelWarn)
	return slog.New(teeHandler{
		newHandler(f, cfg.LogFormat, cfg.LogLevel, false),
		newHandler(os.Stderr, cfg.LogFormat, warnLevel, os.Getenv("JOURNAL_STREAM") != ""),
	}), func() { f.Close() }
}

// newHandler returns a JSON or logfmt slog handler writing to w at level.
// dropTime omits the timestamp (journald records its own).
func newHandler(w io.Writer, format string, level slog.Level, dropTime bool) slog.Handler {
	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if dropTime && len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// teeHandler sends each record to every handler that is enabled for it.
type teeHandler []slog.Handler

// Enabled reports whether any of the handlers accepts level l.
func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

// Handle passes a copy of r to every handler enabled for its level.
func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range t {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a teeHandler whose handlers all carry attrs.
func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

// WithGroup returns a teeHandler whose handlers all open group name.
func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(name)
	}
	return out
}

// printSink implements sink.Sink for -dry-run: it writes the message bodies
// that would be published, one JSON object per line.
type printSink struct {
	enc     *json.Encoder
	logType string
	zone    *time.Location
}

// Commit prints the batch. A closed stdout (e.g. piped into `head`) ends the
// output quietly instead of failing.
func (p printSink) Commit(_ context.Context, b *sink.Batch) (int64, error) {
	for _, m := range rabbitmq.Group(b.Events, p.logType, p.zone) {
		if err := p.enc.Encode(m.Details); err != nil {
			if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) {
				return 0, nil
			}
			return 0, err
		}
	}
	return int64(len(b.Events)), nil
}
