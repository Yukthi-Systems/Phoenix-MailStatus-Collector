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
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/wagslane/go-rabbitmq"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/mailstatus"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/sink"
)

const (
	// publishTimeout bounds one batch, including the wait for confirmations,
	// so a stalled broker surfaces as a logged, retried error.
	publishTimeout = 30 * time.Second

	// returnGrace gives an unroutable message's basic.return time to reach its
	// handler. The broker sends it before the confirmation, but the client
	// delivers the two on different goroutines.
	returnGrace = 20 * time.Millisecond
)

// Options configures a Publisher.
type Options struct {
	URL        string
	Exchange   string // "" is the default exchange: RoutingKey is then the queue name
	RoutingKey string
	Type       string         // LogDetails.Type
	TimeZone   *time.Location // zone of the timestamps; nil: UTC
	HeaderType string         // AMQP header "type": the consumer picks its handler by it ("fuglu")
	AppID      string
	Log        *slog.Logger
}

// Publisher sends events to RabbitMQ.
//
// go-rabbitmq re-establishes the connection and channel by itself, but messages
// sent while the connection dies are lost without an error. So every batch uses
// publisher confirms: Publish succeeds only once the broker has accepted all of
// it. Otherwise the collector retries, and meanwhile the journal holds the backlog.
type Publisher struct {
	opt      Options
	conn     *rabbitmq.Conn
	pub      *rabbitmq.Publisher
	returned atomic.Int64 // messages the broker could not route
}

// Open connects to the broker. Exchanges and queues are not declared: they
// must exist, and a message that reaches no queue is an error, not a silent drop.
func Open(opt Options) (*Publisher, error) {
	logger := slogAdapter{opt.Log}
	conn, err := rabbitmq.NewConn(opt.URL, rabbitmq.WithConnectionOptionsLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("connect to rabbitmq: %w", err)
	}
	pub, err := rabbitmq.NewPublisher(conn,
		rabbitmq.WithPublisherOptionsLogger(logger),
		rabbitmq.WithPublisherOptionsConfirm,
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("create rabbitmq publisher: %w", err)
	}
	p := &Publisher{opt: opt, conn: conn, pub: pub}
	pub.NotifyReturn(func(r rabbitmq.Return) {
		p.returned.Add(1)
		opt.Log.Warn("rabbitmq returned a message: no queue is bound for it",
			"exchange", r.Exchange, "routing_key", r.RoutingKey, "reply", r.ReplyText, "message_id", r.MessageId)
	})
	return p, nil
}

// Close closes the publisher and its connection.
func (p *Publisher) Close() {
	p.pub.Close()
	p.conn.Close()
}

// Publish sends the events and waits until the broker has confirmed all of
// them. On error some may have been delivered, so a retry can send duplicates.
func (p *Publisher) Publish(ctx context.Context, events []mailstatus.Event) error {
	msgs := Group(events, p.opt.Type, p.opt.TimeZone)
	if len(msgs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	p.returned.Store(0)

	var pending rabbitmq.PublisherConfirmation
	for _, m := range msgs {
		body, err := json.Marshal(m.Details)
		if err != nil {
			return fmt.Errorf("encode message: %w", err)
		}
		// The AMQP message_id is the euid (the reference header), or a
		// stable generated ID when the mail has none.
		msgID := m.Details.EUID
		if msgID == "" {
			msgID = m.ID
		}
		conf, err := p.pub.PublishWithDeferredConfirmWithContext(ctx, body, []string{p.opt.RoutingKey},
			rabbitmq.WithPublishOptionsExchange(p.opt.Exchange),
			rabbitmq.WithPublishOptionsContentType("application/json"),
			rabbitmq.WithPublishOptionsPersistentDelivery,
			rabbitmq.WithPublishOptionsMandatory,
			rabbitmq.WithPublishOptionsMessageID(msgID),
			rabbitmq.WithPublishOptionsAppID(p.opt.AppID),
			rabbitmq.WithPublishOptionsTimestamp(time.Now()),
			rabbitmq.WithPublishOptionsHeaders(p.headers()),
		)
		if err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		pending = append(pending, conf...)
	}

	for _, c := range pending {
		// Right after a reconnect the new channel can briefly be out of confirm
		// mode, and the library then returns a nil confirmation.
		if c == nil {
			return errors.New("channel is not in confirm mode yet (reconnecting)")
		}
		acked, err := c.WaitContext(ctx)
		if err != nil {
			return fmt.Errorf("wait for confirmation: %w", err)
		}
		if !acked {
			return errors.New("broker did not confirm a message (rejected or connection lost)")
		}
	}

	select {
	case <-time.After(returnGrace):
	case <-ctx.Done():
		return ctx.Err()
	}
	if n := p.returned.Load(); n > 0 {
		return fmt.Errorf("%d message(s) could not be routed: check the exchange, routing key and queue", n)
	}
	for _, m := range msgs {
		d := m.Details
		p.opt.Log.Info("published", "routing_key", p.opt.RoutingKey, "euid", d.EUID, "status", d.Status,
			"to", d.ToEmailIds, "queue_id", d.MetaData["queue_id"])
	}
	return nil
}

// headers returns the AMQP headers set on every message.
func (p *Publisher) headers() rabbitmq.Table {
	if p.opt.HeaderType == "" {
		return nil
	}
	return rabbitmq.Table{"type": p.opt.HeaderType}
}

// Host returns host:port/vhost of an AMQP URL, without the credentials, for logging.
func Host(amqpURL string) string {
	u, err := url.Parse(amqpURL)
	if err != nil {
		return "(invalid URL)"
	}
	return u.Host + u.Path
}

// EventPublisher is the part of Publisher that Sink needs.
type EventPublisher interface {
	Publish(ctx context.Context, events []mailstatus.Event) error
}

// StateSaver stores the journal position and in-flight messages of a batch.
type StateSaver interface {
	Save(b *sink.Batch) error
}

// Sink writes batches to RabbitMQ and then saves the collector state. State is
// saved only after the broker confirmed, so a crash in between re-reads and
// re-sends those lines: delivery is at-least-once, never lost.
type Sink struct {
	Publisher EventPublisher
	State     StateSaver // nil: state is not saved (backfill)

	published bool // the pending batch is confirmed, only its state save failed
}

// Commit implements sink.Sink. The collector retries a failed batch
// unchanged before it reads more, so after a failed state save it is enough to
// retry the save, without publishing everything again.
func (s *Sink) Commit(ctx context.Context, b *sink.Batch) (int64, error) {
	if !s.published {
		if err := s.Publisher.Publish(ctx, b.Events); err != nil {
			return 0, err
		}
		s.published = true
	}
	if s.State != nil {
		if err := s.State.Save(b); err != nil {
			return 0, err
		}
	}
	s.published = false
	return int64(len(b.Events)), nil
}

// slogAdapter sends go-rabbitmq's log lines to the collector's logger.
type slogAdapter struct{ log *slog.Logger }

// Fatalf exits the process. The library calls it when a publisher cannot recover
// after a reconnect, and carrying on would retry forever with no confirmations.
// Exiting is safe: systemd restarts the collector and it resumes from its saved state.
func (l slogAdapter) Fatalf(format string, v ...any) {
	l.log.Error(fmt.Sprintf(format, v...))
	os.Exit(1)
}

// Errorf logs at error level.
func (l slogAdapter) Errorf(format string, v ...any) { l.log.Error(fmt.Sprintf(format, v...)) }

// Warnf logs at warn level.
func (l slogAdapter) Warnf(format string, v ...any) { l.log.Warn(fmt.Sprintf(format, v...)) }

// Infof logs at info level.
func (l slogAdapter) Infof(format string, v ...any) { l.log.Info(fmt.Sprintf(format, v...)) }

// Debugf logs at debug level.
func (l slogAdapter) Debugf(format string, v ...any) { l.log.Debug(fmt.Sprintf(format, v...)) }
