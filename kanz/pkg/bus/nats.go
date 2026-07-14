package bus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// defaultDrainGrace bounds how long Subscribe's shutdown waits for a JetStream
// pull consumer to drain its local buffer (see Subscribe). Long enough to carry a
// realistic in-flight batch through a downstream call (e.g. a Kafka produce);
// short enough that a handler which ignores context cancellation cannot hang
// shutdown indefinitely.
const defaultDrainGrace = 5 * time.Second

// drainStopSlack is added on top of the drain grace period before Subscribe gives
// up waiting on Drain() and returns anyway. A well-behaved handler that honors ctx
// cancellation returns (and gets Nak'd) within microseconds of the grace deadline,
// so Drain ordinarily finishes just after `grace` elapses — this margin exists
// only to bound the pathological case (a handler that never observes ctx) rather
// than to be routinely used. It does not stop anything by itself; see Subscribe.
const drainStopSlack = 2 * time.Second

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
	// DrainGrace bounds Subscribe's shutdown drain window (default 5s). See
	// Subscribe for why shutdown drains rather than stops.
	DrainGrace time.Duration

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
	if cfg.DrainGrace == 0 {
		cfg.DrainGrace = defaultDrainGrace
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
	// ONE DURABLE PER (GROUP, SUBJECT) — not one per group.
	//
	// This used to be `Durable: group` with `FilterSubject: subject`, through
	// CreateOrUpdate. A service that subscribes to several subjects under one
	// consumer group — which is every service on this platform — therefore created
	// ONE consumer and then OVERWROTE its filter with each subsequent Subscribe.
	// The last call won and every earlier subscription went silently dead: the
	// handler was registered, the consumer existed, nothing errored, and the events
	// were simply never delivered.
	//
	// For the OMS that meant a durable filtered to `order.order.amend` — the last
	// subject it happens to subscribe to — and NO SUBMITTED ORDER WAS EVER
	// PROCESSED on a real spine. It was invisible because CreateOrUpdate is a
	// silent overwrite, and because nothing had ever run the OMS against a real
	// broker (EXEC-M9).
	//
	// The durable name carries the subject so each subscription gets its own
	// consumer. Group semantics are unchanged: every pod of a service still shares
	// one durable PER SUBJECT, so a message goes to exactly one of them.
	cons, err := c.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       durableName(group, subject),
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: subject,
	})
	if err != nil {
		return fmt.Errorf("nats: consumer %q on stream %q: %w", durableName(group, subject), stream, err)
	}
	// handlerCtx is the context handed to h() for each delivery. It starts as ctx
	// and is swapped to a fresh drain context at shutdown (below) — read via
	// atomic.Pointer since it's written from this goroutine and read from the
	// NATS library's delivery goroutine invoking the callback.
	var handlerCtx atomic.Pointer[context.Context]
	handlerCtx.Store(&ctx)

	cc, err := cons.Consume(func(m jetstream.Msg) {
		hctx := *handlerCtx.Load()
		if err := h(hctx, natsToMessage(m)); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("nats: start consume: %w", err)
	}
	<-ctx.Done()

	// Shut down via Drain, not Stop. jetstream.ConsumeContext documents the
	// difference explicitly: Stop "discards" whatever is already in the client's
	// local delivery buffer; Drain "processes" it through the callback. JetStream
	// marks a message delivered — starting its AckWait timer (30s default) — at
	// FETCH time, not callback time, so a message Stop() abandons in the buffer
	// is neither acked nor nacked: the server sits on it for the full AckWait
	// while its cleanly-NACKed siblings redeliver instantly and get reprocessed
	// first. For a per-key-ordered consumer (the archiver, tv-sync's cost-basis
	// fold) that reorders the replayed log on every restart — including an
	// ordinary rolling deploy.
	//
	// The drain window gets its OWN context, not ctx itself: ctx.Done() just
	// fired, and handing an already-cancelled context to h() would make any
	// downstream call it makes (e.g. the archiver's Kafka publish) fail
	// instantly — NAKing the very messages the drain exists to save, a more
	// convoluted way to reproduce the same loss. A fresh context bounded by its
	// own deadline lets a handler that's mid-flight finish real work if it can;
	// anything still running when the deadline hits observes cancellation the
	// same way it would observe any other downstream failure, returns an error,
	// and gets NAK'd through the existing error path above — an EXPLICIT
	// immediate-redelivery NAK, not a silent 30s wait.
	grace := c.cfg.DrainGrace
	if grace <= 0 {
		grace = defaultDrainGrace
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	handlerCtx.Store(&drainCtx)
	cc.Drain()

	select {
	case <-cc.Closed():
		// Every buffered message was handed to h() — acked or, past the drain
		// deadline, NAK'd immediately above. Nothing was left in limbo.
	case <-time.After(grace + drainStopSlack):
		// Drain did not finish within a bounded safety margin past the handler
		// deadline. This is what actually bounds Subscribe's return — not a
		// fallback Stop() call, which would not help here: jetstream's
		// ConsumeContext.Stop() and Drain() share one CompareAndSwap-guarded
		// "closed" flag (nats.go jetstream/pull.go), Drain() above already won
		// it, so a Stop() call at this point no-ops (its own CAS fails and it
		// returns immediately, unsubscribing and discarding nothing).
		//
		// The only way this branch fires is a handler that never observes
		// drainCtx cancellation and is stuck mid-call. Neither Stop() nor
		// Drain() can preempt a goroutine blocked inside a synchronous
		// callback, so that handler invocation keeps running on the NATS
		// client's per-subscription dispatch goroutine, and jetstream's
		// internal consumer-monitor goroutine stays up alongside it, until the
		// handler eventually returns or the process exits. Subscribe returning
		// here does not reclaim either goroutine — it only stops this function
		// from blocking shutdown on a handler that will not cooperate. The
		// real backstop for that case is the process being killed (the pod's
		// terminationGracePeriodSeconds), not this select.
	}
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

// durableName builds the JetStream durable for one (group, subject) pair.
//
// A durable name may not contain `.`, `*`, `>`, or whitespace, so the subject's
// dots become underscores: group "oms" + subject "order.order.submit" ⇒
// "oms-order_order_submit". The name is deterministic, so every replica of a
// service binds to the SAME durable for a subject and the messages are shared
// between them — which is what a consumer group means.
func durableName(group, subject string) string {
	safe := strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(subject)
	return group + "-" + safe
}

// SubscribeBroadcast implements BroadcastSubscriber: an EPHEMERAL consumer starting
// at the subject's LAST message.
//
// Ephemeral (no Durable) so every pod gets its OWN consumer and therefore its own
// copy — a durable group would hand the message to exactly one of them, which is
// what left one webhook-ingest replica trading while another stayed halted.
//
// DeliverLastPerSubject so a pod that starts LEARNS THE CURRENT STATE immediately,
// rather than resuming a durable past the only message that mattered. This is what
// makes a restart safe: the last operator-declared mode is re-applied, whatever it
// was. If it was HALTED, the pod comes back halted — the lifecycle contract that
// HALTED requires intervention to leave is preserved, because a replay of the halt
// FACT is not an intervention, it is the truth.
func (c *NATSClient) SubscribeBroadcast(ctx context.Context, subject string, h Handler) error {
	return c.SubscribeBroadcastReady(ctx, subject, h, nil)
}

// SubscribeBroadcastReady is SubscribeBroadcast with an ARMED signal: ready fires once the
// backlog that existed at subscribe time has been delivered and acked, so a caller can hold
// /readyz closed until it actually knows the state it is about to act on.
func (c *NATSClient) SubscribeBroadcastReady(ctx context.Context, subject string, h Handler, ready func()) error {
	stream, err := c.js.StreamNameBySubject(ctx, subject)
	if err != nil {
		return fmt.Errorf("nats: stream for subject %q: %w", subject, err)
	}
	cons, err := c.js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverLastPerSubjectPolicy,
		// The server reaps the ephemeral consumer once the pod is gone.
		InactiveThreshold: 5 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("nats: broadcast consumer on %q: %w", subject, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := h(ctx, natsToMessage(m)); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return fmt.Errorf("nats: start broadcast consume: %w", err)
	}

	// ARMED = the replay that existed when we subscribed has been delivered AND acked.
	// NumPending alone is not enough: the server can have nothing left to send while the
	// handler is still folding what it already sent, and a caller that opened /readyz then
	// would act on a half-learned book.
	if ready != nil {
		go func() {
			t := time.NewTicker(50 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					info, err := cons.Info(ctx)
					if err != nil {
						continue // transient; keep waiting rather than declare a book we do not have
					}
					if info.NumPending == 0 && info.NumAckPending == 0 {
						ready()
						return
					}
				}
			}
		}()
	}

	<-ctx.Done()
	cc.Stop()
	return nil
}

var _ BroadcastSubscriber = (*NATSClient)(nil)
