package bus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
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

	// ConsumerTuning overrides the per-subject delivery contract
	// (tuningForSubject) for EVERY durable this client creates. Leave it nil —
	// the built-in classes are the answer for production, and the reason they
	// live in tuning.go instead of at twenty composition roots is that twenty
	// copies of three numbers is how a fix stops spreading.
	//
	// It exists for the cases that genuinely cannot use them: an integration test
	// that needs a 2s AckWait to observe a redelivery inside a test's lifetime,
	// or a future service that can SHOW its handler does not fit a class. DialNATS
	// validates it (ConsumerTuning.validate) and refuses to dial on a bad one,
	// including one whose AckWait would reach the dedup claim lease and re-invert
	// the duplicate-dispatch guard — a misconfiguration here must surface as a
	// process that will not start, not as a delivery pathology in production.
	ConsumerTuning *ConsumerTuning

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

	// Logger receives diagnostics this client cannot surface any other way — most
	// importantly a failed Ack/Nak inside the Consume callback (see Subscribe and
	// SubscribeBroadcastReady), which happens below the wrapped Handler and below
	// the Consumer layer where BusMetrics lives, so nothing upstream can observe it
	// otherwise. nil defaults to slog.Default() in DialNATS. Every service already
	// calls slog.SetDefault(logger) in main (see services/oms/cmd/oms/main.go), so
	// leaving this unset still routes into the structured log pipeline with no
	// call-site changes; set it explicitly only if a client needs its own logger.
	Logger *slog.Logger
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ConsumerTuning != nil {
		if err := cfg.ConsumerTuning.validate(); err != nil {
			return nil, fmt.Errorf("nats: ConsumerTuning: %w", err)
		}
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
	//
	// CREATE-OR-UPDATE STAYS AN OVERWRITE, AND THAT IS THE DECISION (#237).
	//
	// The alternative was to detect drift and refuse to start, treating a
	// hand-edited consumer as authoritative. It was rejected: the durable's
	// delivery contract has to match the code that handles the deliveries, and a
	// pod that will not boot because someone ran `nats consumer edit` during an
	// incident turns a tuning experiment into an outage. The contract is
	// config-as-code — tuning.go plus a deploy — and an operator edit is
	// transient BY DESIGN.
	//
	// What was NOT acceptable was that the revert was silent: a responder who
	// raised AckWait to 120s mid-incident got it reset by the next rolling deploy
	// with no error and no log line, and nothing anywhere said so. logTuningDrift
	// below reads the existing consumer first and logs a WARN naming each field it
	// is about to overwrite and its old value. Transient by design is fine;
	// transient and invisible is how the same incident gets diagnosed twice.
	//
	// AckWait / MaxDeliver / MaxAckPending are set EXPLICITLY, from
	// tuningForSubject (see tuning.go for each number and the reasoning behind
	// it). Leaving them unset — which is what this call did until #237 — is not
	// "the defaults are fine", it is three JetStream server defaults nobody chose:
	// unbounded redelivery of a poison message, an AckWait shorter than a slow
	// venue round-trip, and 1000 messages queued in front of a single dispatch
	// goroutine whose ack clock is already running. test/arch's
	// TestJetStreamConsumersAreExplicitlyTuned fails the build if they go missing
	// again.
	tuning := tuningForSubject(subject)
	if c.cfg.ConsumerTuning != nil {
		tuning = *c.cfg.ConsumerTuning
	}
	want := jetstream.ConsumerConfig{
		Durable:       durableName(group, subject),
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: subject,
		AckWait:       tuning.AckWait,
		MaxDeliver:    tuning.MaxDeliver,
		MaxAckPending: tuning.MaxAckPending,
	}
	c.logTuningDrift(ctx, stream, want)
	cons, err := c.js.CreateOrUpdateConsumer(ctx, stream, want)
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
		// dispatchMessage, not h: a panic in a direct Subscribe caller's handler
		// would otherwise unwind through this callback and kill the process, taking
		// every other subscription in it down too. Consumer recovers above this, so
		// for a Consumer-backed subscription the recover here never fires; this is
		// the net for direct callers (today: the archiver).
		if err := dispatchMessage(hctx, h, natsToMessage(m)); err != nil {
			// Surfaced deliberately (logNakFailure) but never escalated further: the
			// handler already decided this delivery failed, and a Nak that itself
			// fails to reach the broker still redelivers — just later, on the
			// AckWait timeout, instead of on the backoff. Nothing here should turn
			// into a panic or a different return; the delivery outcome is already
			// decided.
			//
			// NakWithDelay, NOT Nak. See nakDelay in tuning.go: a bare Nak triggers
			// INSTANT redelivery — it honours neither AckWait nor a consumer BackOff
			// — so a handler failing fast against a downed dependency spins the
			// message thousands of times a second and burns a finite MaxDeliver in
			// under a millisecond, while the outage is still in its first second.
			if nakErr := m.NakWithDelay(nakDelayFor(m)); nakErr != nil {
				logNakFailure(c.cfg.Logger, subject, group, nakErr)
			}
			return
		}
		// Surfaced deliberately (logAckFailure) but MUST NEVER become a Nak or an
		// early return: the handler already succeeded, the work is done. Nak-ing a
		// message whose handler succeeded would ask the broker to redeliver work
		// that already happened — the wrong failure mode in the other direction.
		if ackErr := m.Ack(); ackErr != nil {
			logAckFailure(c.cfg.Logger, subject, group, ackErr)
		}
	})
	if err != nil {
		return fmt.Errorf("nats: start consume: %w", err)
	}
	<-ctx.Done()

	// Shut down via Drain, not Stop. jetstream.ConsumeContext documents the
	// difference explicitly: Stop "discards" whatever is already in the client's
	// local delivery buffer; Drain "processes" it through the callback. JetStream
	// marks a message delivered — starting its AckWait timer (tuning.go: 60s for work
	// subjects, 15s for market.>) — at
	// FETCH time, not callback time, so a message Stop() abandons in the buffer
	// is neither acked nor nacked: the server sits on it for the full AckWait
	// while its cleanly-NACKed siblings redeliver on nakDelay's backoff (2s for a
	// first failure) and get reprocessed
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
	// and gets NAK'd through the existing error path above — an EXPLICIT NAK on
	// nakDelay's backoff (2s for a first failure), not a silent wait for the full
	// AckWait.
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

// Pending returns the number of messages waiting to be delivered to the
// durable consumer for (subject, group) — the same (stream, durable) pair
// Subscribe binds. It is the lag signal for a subscriber whose real failure
// mode is falling behind, not going down (the archiver, DATA-M1): being down
// is visible and self-healing; being slow is invisible without this.
//
// It looks up the EXISTING consumer rather than creating one — Pending is
// meant to be polled alongside an already-running Subscribe, not to conjure a
// consumer of its own — so it returns an error until Subscribe has run at
// least once for this (subject, group).
func (c *NATSClient) Pending(ctx context.Context, subject, group string) (int64, error) {
	stream, err := c.js.StreamNameBySubject(ctx, subject)
	if err != nil {
		return 0, fmt.Errorf("nats: stream for subject %q: %w", subject, err)
	}
	cons, err := c.js.Consumer(ctx, stream, durableName(group, subject))
	if err != nil {
		return 0, fmt.Errorf("nats: consumer %q on stream %q: %w", durableName(group, subject), stream, err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("nats: consumer info for %q: %w", durableName(group, subject), err)
	}
	return int64(info.NumPending), nil
}

// BacklogKind reports that this transport's backlog lands in
// kanz_bus_pending_messages. See backlog.go.
func (c *NATSClient) BacklogKind() BacklogKind { return BacklogPending }

// Backlog implements BacklogSource over Pending. JetStream has no partitions,
// so it is always exactly one reading with an empty Partition.
//
// The error is returned rather than folded into a zero on purpose: Pending fails
// when the stream lookup, the consumer lookup or ConsumerInfo fails, and every
// one of those means the broker did not answer — not that there is nothing
// waiting. pollBacklog turns that into a DELETED series.
func (c *NATSClient) Backlog(ctx context.Context, subject, group string) ([]PartitionBacklog, error) {
	n, err := c.Pending(ctx, subject, group)
	if err != nil {
		return nil, err
	}
	return []PartitionBacklog{{Messages: n}}, nil
}

var _ BacklogSource = (*NATSClient)(nil)

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

// logAckFailure reports that JetStream rejected the Ack for a message whose
// handler already ran to completion. This is NOT data loss and must never be
// treated as one — the work is done — but the broker was never told, so it
// will redeliver this exact message forever, and the handler will redo the
// same work forever, while every health signal this service exports keeps
// reporting green. The only symptom is load.
//
// The most likely cause: Msg.Ack() publishes to $JS.ACK.>, a subject space
// distinct from the $JS.API.> used to manage the consumer itself. A
// permissions grant of `publish: ["$JS.API.>"]` alone LOOKS sufficient to run
// a JetStream consumer — the consumer creates and binds fine — but every Ack
// silently fails from the first message onward until `$JS.ACK.>` is also
// granted.
//
// logger nil-checked rather than trusted, so a caller that builds a
// NATSClient by hand (bypassing DialNATS's default) cannot nil-panic here of
// all places — the one path that exists purely to report a problem must not
// itself become one.
func logAckFailure(logger *slog.Logger, subject, group string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("jetstream ack failed: handler already completed successfully but the broker was never told, "+
		"so this message will be redelivered and reprocessed indefinitely while the service reports healthy — "+
		"check for a missing $JS.ACK.> publish grant on this service's NATS user",
		"subject", subject,
		"group", group,
		"err", err,
	)
}

// logNakFailure reports that JetStream rejected the Nak for a message whose
// handler failed. Less severe than a failed Ack: the message still
// redelivers once its AckWait timer expires (tuning.go: 60s for work subjects,
// 15s for market.>) — the Nak only makes
// that happen sooner, on nakDelay's backoff. Logged for the same underlying reason
// (see logAckFailure): a broker-side permission gap that breaks
// acknowledgement should never be silent.
func logNakFailure(logger *slog.Logger, subject, group string, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("jetstream nak failed: redelivery will now wait for the ack-wait timeout instead of retrying "+
		"immediately — check for a missing $JS.ACK.> publish grant on this service's NATS user",
		"subject", subject,
		"group", group,
		"err", err,
	)
}

// logTuningDrift reports, BEFORE CreateOrUpdateConsumer overwrites it, every
// delivery-contract field on an existing durable that differs from what this
// process is about to write.
//
// It exists because the overwrite is deliberate (see Subscribe) but used to be
// invisible. An operator who raises AckWait on a consumer during an incident is
// making a temporary change to a value this code owns; the next pod restart
// silently reverts it, and the only trace was the incident recurring. One WARN
// naming the field, the old value and the new one turns that into something a
// responder can find in the log of the pod that did it.
//
// Everything here is best-effort and non-fatal. A consumer that does not exist
// yet (the common case on a fresh spine) returns an error from js.Consumer and
// there is nothing to report; an Info call that fails costs a log line, never a
// subscription. It adds one $JS.API round-trip per Subscribe at startup — paid
// once per (service, subject), not per message.
func (c *NATSClient) logTuningDrift(ctx context.Context, stream string, want jetstream.ConsumerConfig) {
	existing, err := c.js.Consumer(ctx, stream, want.Durable)
	if err != nil {
		return // not provisioned yet — nothing is being overwritten
	}
	info, err := existing.Info(ctx)
	if err != nil {
		return
	}
	var drift []any
	if info.Config.AckWait != want.AckWait {
		drift = append(drift, "ack_wait_was", info.Config.AckWait, "ack_wait_now", want.AckWait)
	}
	if info.Config.MaxDeliver != want.MaxDeliver {
		drift = append(drift, "max_deliver_was", info.Config.MaxDeliver, "max_deliver_now", want.MaxDeliver)
	}
	if info.Config.MaxAckPending != want.MaxAckPending {
		drift = append(drift, "max_ack_pending_was", info.Config.MaxAckPending, "max_ack_pending_now", want.MaxAckPending)
	}
	if len(drift) == 0 {
		return
	}
	logger := c.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("jetstream consumer tuning is being OVERWRITTEN by this process: the durable's delivery contract "+
		"is config-as-code (pkg/bus/tuning.go) and CreateOrUpdateConsumer reasserts it on every start, so any "+
		"value set out-of-band with `nats consumer edit` is transient by design — if one of these was an "+
		"incident mitigation it has just been reverted, and the durable fix is a code change plus a deploy",
		append([]any{"stream", stream, "durable", want.Durable}, drift...)...)
}

// nakDelayFor reads the broker's own delivery count for m and returns the
// backoff to NAK with. Metadata() fails only for a message that did not come
// from JetStream, which cannot happen on these callbacks; the fallback is the
// FIRST-delivery delay rather than zero, because zero is instant redelivery —
// the behaviour nakDelay exists to remove — and an error here must not silently
// restore it.
func nakDelayFor(m jetstream.Msg) time.Duration {
	md, err := m.Metadata()
	if err != nil || md == nil {
		return nakDelay(1)
	}
	return nakDelay(md.NumDelivered)
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
	// controlTuning, NOT tuningForSubject: a broadcast carries the halt FACT and
	// the trading mode, and its MaxDeliver is deliberately unbounded where every
	// queue-group durable's is finite. See controlTuning in tuning.go — a bounded
	// MaxDeliver here would let "I could not read the brake signal" stop being
	// re-offered, which resolves to "carry on trading". A NATSConfig override
	// still wins, so a test can shorten the AckWait.
	tuning := controlTuning
	if c.cfg.ConsumerTuning != nil {
		tuning = *c.cfg.ConsumerTuning
	}
	cons, err := c.js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverLastPerSubjectPolicy,
		AckWait:       tuning.AckWait,
		MaxDeliver:    tuning.MaxDeliver,
		MaxAckPending: tuning.MaxAckPending,
		// The server reaps the ephemeral consumer once the pod is gone.
		InactiveThreshold: 5 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("nats: broadcast consumer on %q: %w", subject, err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		if err := h(ctx, natsToMessage(m)); err != nil {
			// See the queue-group Subscribe callback above: surfaced deliberately,
			// never escalated. A failed Nak still redelivers on the AckWait timeout.
			// NakWithDelay for the same reason as the queue-group path above: a bare
			// Nak is instant redelivery, which against a persistently unreadable
			// control message is a hot loop rather than a retry.
			if nakErr := m.NakWithDelay(nakDelayFor(m)); nakErr != nil {
				logNakFailure(c.cfg.Logger, subject, "", nakErr)
			}
			return
		}
		// See the queue-group Subscribe callback above: surfaced deliberately, and
		// MUST NEVER become a Nak or an early return — the handler already
		// succeeded and the work is done.
		if ackErr := m.Ack(); ackErr != nil {
			logAckFailure(c.cfg.Logger, subject, "", ackErr)
		}
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
