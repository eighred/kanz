package bus

import (
	"context"
	"crypto/tls"
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

	// TLSConfig enables TLS (SEC-01c). For the zero-trust mesh the caller
	// builds it from the workload SVID via transport.ClientTLSConfig
	// (SEC-01b) so the connection is mutually authenticated and the NATS
	// server's SPIFFE identity is verified — the client cert is also the
	// auth credential (NATS maps the verified cert to a user). nil ⇒
	// plaintext, for local/dev only. bus stays decoupled from go-spiffe by
	// taking a ready *tls.Config rather than a SPIFFE source.
	TLSConfig *tls.Config
	// Username/Password/Token are NATS-native auth, an alternative to
	// mTLS-cert auth for deployments not yet on SPIFFE. Username+Password
	// are used together; Token takes precedence when set.
	Username string
	Password string
	Token    string

	// OnDisconnect fires when the spine is lost. MaxReconnects defaults to -1
	// (retry forever), which means a disconnect is otherwise SILENT: the client
	// buffers and heals and nothing upstream ever learns the bus went away. A
	// trading process must learn — losing the spine means fills and the halt FACT
	// itself stop flowing, so it can no longer know whether it is safe to trade.
	// Wire this to translate.Gate.TripOnBusLoss to fail closed. err is nil on a
	// clean close. Called on a NATS goroutine: do not block.
	OnDisconnect func(err error)
	// OnReconnect fires when the spine returns. It deliberately does NOT reopen the
	// halt gate — the wire is back, which is not the same as the book being safe.
	// Use it for logging and metrics.
	OnReconnect func()
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
	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.Timeout(cfg.ConnectTimeout),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.MaxReconnects(cfg.MaxReconnects),
	}
	if cfg.OnDisconnect != nil {
		fn := cfg.OnDisconnect
		opts = append(opts,
			nats.DisconnectErrHandler(func(_ *nats.Conn, err error) { fn(err) }),
			// A client that exhausts MaxReconnects closes for good without ever firing
			// DisconnectErrHandler again, so the terminal case gets its own hook —
			// otherwise the one disconnect that is permanent is the one we miss.
			nats.ClosedHandler(func(c *nats.Conn) { fn(c.LastError()) }),
		)
	}
	if cfg.OnReconnect != nil {
		fn := cfg.OnReconnect
		opts = append(opts, nats.ReconnectHandler(func(*nats.Conn) { fn() }))
	}
	if cfg.TLSConfig != nil {
		opts = append(opts, nats.Secure(cfg.TLSConfig))
	}
	switch {
	case cfg.Token != "":
		opts = append(opts, nats.Token(cfg.Token))
	case cfg.Username != "":
		opts = append(opts, nats.UserInfo(cfg.Username, cfg.Password))
	}
	conn, err := nats.Connect(cfg.URL, opts...)
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
