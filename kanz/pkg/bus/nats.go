package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsKeyHeader carries Message.Key over NATS, which lacks a native
// partition-key concept (Kafka has one). The receive path strips this
// header from the user-visible Headers map and surfaces it as Message.Key.
const natsKeyHeader = "Kanz-Partition-Key"

var _ Client = (*NATSClient)(nil)

type NATSConfig struct {
	URL            string        // nats://host:4222 (comma-separated for cluster)
	Name           string        // client identifier
	ConnectTimeout time.Duration // default 10s
	ReconnectWait  time.Duration // default 2s
	MaxReconnects  int           // default -1 (forever); set finite for fail-fast
	PublishTimeout time.Duration // default 5s
}

type NATSClient struct {
	cfg  NATSConfig
	conn *nats.Conn
	js   jetstream.JetStream
}

func DialNATS(_ context.Context, cfg NATSConfig) (*NATSClient, error) {
	if cfg.URL == "" {
		return nil, errors.New("nats: URL required")
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.ReconnectWait == 0 {
		cfg.ReconnectWait = 2 * time.Second
	}
	if cfg.MaxReconnects == 0 {
		cfg.MaxReconnects = -1
	}
	if cfg.PublishTimeout == 0 {
		cfg.PublishTimeout = 5 * time.Second
	}
	conn, err := nats.Connect(cfg.URL,
		nats.Name(cfg.Name),
		nats.Timeout(cfg.ConnectTimeout),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.MaxReconnects(cfg.MaxReconnects),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("nats jetstream: %w", err)
	}
	return &NATSClient{cfg: cfg, conn: conn, js: js}, nil
}

func (c *NATSClient) Publish(ctx context.Context, msg Message) error {
	nm := &nats.Msg{
		Subject: msg.Subject,
		Data:    msg.Body,
		Header:  make(nats.Header, len(msg.Headers)+1),
	}
	for k, v := range msg.Headers {
		nm.Header.Set(k, v)
	}
	if len(msg.Key) > 0 {
		nm.Header.Set(natsKeyHeader, string(msg.Key))
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.PublishTimeout)
	defer cancel()
	if _, err := c.js.PublishMsg(ctx, nm); err != nil {
		return fmt.Errorf("nats publish: %w", err)
	}
	return nil
}

func (c *NATSClient) Subscribe(ctx context.Context, subject, group string, h Handler) error {
	stream, err := c.js.StreamNameBySubject(ctx, subject)
	if err != nil {
		return fmt.Errorf("nats: stream for subject %q: %w", subject, err)
	}
	cons, err := c.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       group,
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: subject,
	})
	if err != nil {
		return fmt.Errorf("nats: consumer %q on stream %q: %w", group, stream, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := h(ctx, natsToMessage(m)); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("nats: start consume: %w", err)
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

func (c *NATSClient) Close() error {
	if c.conn != nil {
		c.conn.Close()
	}
	return nil
}

func natsToMessage(m jetstream.Msg) Message {
	hdr := m.Headers()
	out := Message{Subject: m.Subject(), Body: m.Data()}
	if len(hdr) == 0 {
		return out
	}
	h := make(map[string]string, len(hdr))
	for k, vs := range hdr {
		if len(vs) == 0 {
			continue
		}
		if k == natsKeyHeader {
			out.Key = []byte(vs[0])
			continue
		}
		h[k] = vs[0]
	}
	if len(h) > 0 {
		out.Headers = h
	}
	return out
}
