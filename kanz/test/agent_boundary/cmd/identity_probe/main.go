// identity_probe is a bounded development fixture, not a released command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

const streamName = "AGENT_IDENTITY"

var deniedPublications = []struct{ Case, Subject string }{
	{"order_submit_denied", "order.order.submit"},
	{"order_cancel_denied", "order.order.cancel"},
	{"order_amend_denied", "order.order.amend"},
	{"order_approve_denied", "order.order.approve"},
	{"tenant_order_submit_denied", "tenant.acme.order.order.submit"},
	{"tenant_order_cancel_denied", "tenant.acme.order.order.cancel"},
	{"tenant_order_amend_denied", "tenant.acme.order.order.amend"},
	{"tenant_order_approve_denied", "tenant.acme.order.order.approve"},
	{"mandate_policy_denied", "compliance.mandate.changed.acme.fixture"},
	{"risk_policy_denied", "risk.portfolio.revalued"},
	{"kill_switch_denied", "platform.mode.changed"},
	{"jetstream_stream_create_denied", "$JS.API.STREAM.CREATE.AGENT_FORBIDDEN"},
	{"jetstream_stream_delete_denied", "$JS.API.STREAM.DELETE.AGENT_IDENTITY"},
	{"jetstream_consumer_create_denied", "$JS.API.CONSUMER.CREATE.AGENT_IDENTITY"},
}

var deniedSubscriptions = []struct{ Case, Subject string }{
	{"order_subscription_denied", "order.>"},
	{"risk_subscription_denied", "risk.>"},
	{"mandate_subscription_denied", "compliance.mandate.>"},
	{"foreign_tenant_subscription_denied", "tfact.acme.order.>"},
}

type result struct {
	Passed     bool            `json:"passed"`
	Cases      map[string]bool `json:"cases"`
	FailedCase string          `json:"failedCase,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Identity   string          `json:"identity,omitempty"`
	Retained   uint64          `json:"retained,omitempty"`
}

func main() {
	mode := flag.String("mode", "", "owned fixture operation")
	address := flag.String("nats", "", "private service address")
	namespace := flag.String("namespace", "kanz-services", "expected workload namespace")
	sa := flag.String("service-account", "copilot", "expected workload ServiceAccount")
	flag.Parse()
	if *mode == "network-listener" {
		if err := serveInert(); err != nil {
			os.Exit(1)
		}
		return
	}
	out := result{Cases: map[string]bool{}}
	err := run(*mode, *address, *namespace, *sa, &out)
	out.Passed = err == nil
	if err != nil && out.Reason == "" {
		// Deliberately omit raw process, certificate and broker diagnostics.
		out.Reason = "fixture_operation_refused"
	}
	data, _ := json.Marshal(out)
	fmt.Printf("AGENT_IDENTITY_RESULT:%s\n", data)
	if err != nil {
		os.Exit(1)
	}
}

func privateAddress(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "tls" && u.User == nil && u.Port() == "4222" &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == "" && net.ParseIP(u.Hostname()).IsPrivate()
}

func run(mode, address, namespace, sa string, out *result) error {
	if !privateAddress(address) {
		return errors.New("private fixture address required")
	}
	if mode == "network-positive" || mode == "network-denied" {
		if !(namespace == "kanz-services" && sa == "copilot" || namespace == "kanz-messaging" && sa == "nats-bootstrap") {
			return errors.New("unknown fixture identity")
		}
		return networkCheck(mode, address, out)
	}
	validIdentity := namespace == "kanz-services" && sa == "copilot" && (mode == "denied" || mode == "wrong-control") ||
		namespace == "kanz-services" && sa == "agent-identity-unregistered" && mode == "unregistered" ||
		namespace == "kanz-messaging" && sa == "nats-bootstrap" && (mode == "bootstrap" || mode == "verify" || mode == "mutation-verify")
	if !validIdentity {
		return errors.New("unknown fixture identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	source, err := transport.NewSource(ctx, "unix:///run/spiffe/spire-agent.sock")
	if err != nil {
		return err
	}
	defer source.Close()
	svid, err := source.GetX509SVID()
	if err != nil {
		return err
	}
	want, err := transport.ServiceID(namespace, sa)
	if err != nil || svid.ID != want {
		return errors.New("workload identity mismatch")
	}
	out.Identity = svid.ID.String()
	if sa == "copilot" {
		out.Cases["copilot_svid_issued"] = true
	}
	brokerSA := "nats"
	if mode == "wrong-control" {
		brokerSA = "nats-wrong"
	}
	brokerID, err := transport.ServiceID("kanz-messaging", brokerSA)
	if err != nil {
		return err
	}
	asyncErrors := make(chan error, 32)
	nc, err := nats.Connect(address,
		nats.Name("agent-identity-fixture"), nats.Timeout(5*time.Second), nats.NoReconnect(),
		nats.Secure(transport.ClientTLSConfig(source, transport.AuthorizeServices(brokerID))),
		nats.PermissionErrOnSubscribe(true),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case asyncErrors <- err:
			default:
			}
		}),
	)
	if mode == "unregistered" {
		if nc != nil {
			nc.Close()
		}
		if !errors.Is(err, nats.ErrAuthorization) {
			return errors.New("explicit broker auth refusal missing")
		}
		out.Cases["unregistered_identity_denied"] = true
		return nil
	}
	if err != nil {
		return err
	}
	defer nc.Close()
	if err := nc.FlushTimeout(4 * time.Second); err != nil {
		return err
	}
	if mode == "wrong-control" {
		out.Cases["wrong_server_positive_control"] = true
		return nil
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	switch mode {
	case "bootstrap":
		_, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: streamName,
			Subjects: []string{"platform.authz.decision", "platform.agent.answered", "order.>", "tenant.>", "risk.>", "compliance.>", "platform.mode.changed"},
			Storage:  jetstream.MemoryStorage, Replicas: 1, MaxMsgs: 64, MaxMsgSize: 1 << 20})
		return err
	case "verify":
		return verifyRetained(ctx, js, out)
	case "mutation-verify":
		stream, err := js.Stream(ctx, streamName)
		if err != nil {
			return err
		}
		msg, err := stream.GetMsg(ctx, 1)
		if err != nil || msg.Subject != "order.order.submit" || string(msg.Data) != "agent-identity-forbidden" {
			return errors.New("mutation did not retain the forbidden publication")
		}
		info, err := stream.Info(ctx)
		if err != nil || info.State.Msgs != 1 {
			return errors.New("mutation count changed")
		}
		out.Cases["grant_widening_detected"] = true
		out.Retained = info.State.Msgs
		return nil
	case "denied":
		for _, item := range deniedPublications {
			out.FailedCase = item.Case
			if err := expectPublishDenial(nc, asyncErrors, item.Subject); err != nil {
				out.Reason = "publication_succeeded_or_explicit_denial_missing"
				return err
			}
			out.Cases[item.Case] = true
		}
		for _, item := range deniedSubscriptions {
			out.FailedCase = item.Case
			if err := expectSubscribeDenial(nc, asyncErrors, item.Subject); err != nil {
				return err
			}
			out.Cases[item.Case] = true
		}
		out.FailedCase = ""
		for _, name := range []string{"capital_publish_denied", "control_publish_denied", "jetstream_admin_denied", "protected_subscription_denied"} {
			out.Cases[name] = true
		}
		return nil
	default:
		return errors.New("unknown fixture operation")
	}
}

const inertResponse = "agent-identity-owned-inert\n"

func serveInert() error {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{Port: 4222})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(180 * time.Second)); err != nil {
		return err
	}
	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				return nil
			}
			return err
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(conn, inertResponse)
		_ = conn.Close()
	}
}

func networkCheck(mode, address string, out *result) error {
	u, _ := url.Parse(address) // already constrained to a literal private IP
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if mode == "network-denied" {
		if conn != nil {
			_ = conn.Close()
		}
		var networkError net.Error
		if err == nil || !errors.As(err, &networkError) || !networkError.Timeout() {
			return errors.New("expected Cilium egress timeout missing")
		}
		out.Cases["probe_egress_denied"] = true
		return nil
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	body := make([]byte, len(inertResponse))
	if _, err := io.ReadFull(conn, body); err != nil || string(body) != inertResponse {
		return errors.New("owned inert destination positive control missing")
	}
	out.Cases["probe_egress_positive_control"] = true
	return nil
}

func expectPublishDenial(nc *nats.Conn, asyncErrors <-chan error, subject string) error {
	if err := nc.Publish(subject, []byte("agent-identity-forbidden")); err != nil {
		return err
	}
	if err := nc.FlushTimeout(4 * time.Second); err != nil {
		return err
	}
	return permissionError(asyncErrors, subject)
}

func expectSubscribeDenial(nc *nats.Conn, asyncErrors <-chan error, subject string) (resultErr error) {
	sub, err := nc.SubscribeSync(subject)
	if err != nil {
		return err
	}
	defer func() {
		// An already-invalid subscription cannot be unsubscribed again; the
		// enclosing connection also closes. Preserve other cleanup failures.
		if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrBadSubscription) {
			resultErr = errors.Join(resultErr, fmt.Errorf("subscription cleanup failed: %w", err))
		}
	}()
	if err := nc.FlushTimeout(4 * time.Second); err != nil {
		return err
	}
	return permissionError(asyncErrors, subject)
}

func permissionError(asyncErrors <-chan error, subject string) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-asyncErrors:
		if !errors.Is(err, nats.ErrPermissionViolation) || !strings.Contains(err.Error(), subject) {
			return errors.New("wrong permission refusal")
		}
		return nil
	case <-timer.C:
		return errors.New("explicit permission refusal missing")
	}
}

func verifyRetained(ctx context.Context, js jetstream.JetStream, out *result) error {
	stream, err := js.Stream(ctx, streamName)
	if err != nil {
		return err
	}
	info, err := stream.Info(ctx)
	if err != nil || info.State.Msgs != 3 {
		return errors.New("authorized retained event count differs")
	}
	verdicts := map[string]bool{}
	answers := 0
	for seq := uint64(1); seq <= 3; seq++ {
		msg, err := stream.GetMsg(ctx, seq)
		if err != nil {
			return err
		}
		env, payload, err := bus.Unframe(msg.Data)
		if err != nil || bus.Validate(env) != nil || env.GetTenantId() != "fixture-tenant-a" || env.GetSource() != "copilot" {
			return errors.New("retained envelope is not the copilot's canonical event")
		}
		switch msg.Subject {
		case "platform.authz.decision":
			var value observationpb.DecisionLog
			if proto.Unmarshal(payload, &value) != nil || value.GetDecider() != "copilot" ||
				value.GetAttributes()["principal.subject"] != "agent-identity-analyst" ||
				value.GetAttributes()["principal.tenant"] != "fixture-tenant-a" ||
				env.GetPayloadSchemaRef() != "observation.v1.DecisionLog:1" {
				return errors.New("retained authorization attribution differs")
			}
			verdicts[value.GetAttributes()["decision"]] = true
		case "platform.agent.answered":
			var value observationpb.AgentAnswerRecord
			if proto.Unmarshal(payload, &value) != nil || value.GetAnswerId() != "agent-identity-answer" ||
				value.GetTenantId() != "fixture-tenant-a" || value.GetPrincipalSubject() != "agent-identity-analyst" ||
				value.GetAgent() != "copilot" || env.GetPayloadSchemaRef() != "observation.v1.AgentAnswerRecord:1" {
				return errors.New("retained answer attribution differs")
			}
			answers++
		default:
			return errors.New("forbidden publication reached retained state")
		}
	}
	if len(verdicts) != 2 || !verdicts["allow"] || !verdicts["deny"] || answers != 1 {
		return errors.New("missing canonical allow, deny or answer")
	}
	out.Cases["authz_publish_allowed"], out.Cases["answer_publish_allowed"] = true, true
	out.Retained = info.State.Msgs
	return nil
}
