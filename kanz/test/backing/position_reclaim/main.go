//go:build linux

// position_reclaim owns one sentinel across two independent go test processes.
// It is a disposable CI fixture, absent from services and release images.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/platform/subject"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const producerSource = "fixture/position-reclaim-proof"

type receipt struct {
	Schema            int       `json:"schema"`
	Phase             string    `json:"phase"`
	Source            string    `json:"source"`
	Run               string    `json:"run"`
	Attempt           string    `json:"attempt"`
	Nonce             string    `json:"nonce"`
	ServerID          string    `json:"server_id"`
	ServerVersion     string    `json:"server_version"`
	StreamCreated     time.Time `json:"stream_created"`
	MaxAge            int64     `json:"max_age"`
	MaxPerSubject     int64     `json:"max_msgs_per_subject"`
	SubjectCount      uint64    `json:"subject_count"`
	SentinelSubject   string    `json:"sentinel_subject"`
	SentinelSequence  uint64    `json:"sentinel_sequence"`
	SentinelSHA256    string    `json:"sentinel_sha256"`
	CleanupAfterCount *uint64   `json:"cleanup_after_count,omitempty"`
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var positiveID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// Every component is opened relative to a retained no-follow directory handle.
func parentFD(path string) (int, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return -1, "", errors.New("canonical absolute receipt path required")
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/")
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	for _, component := range parts[:len(parts)-1] {
		next, err := syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return -1, "", err
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func readReceipt(path string) (receipt, error) {
	var result receipt
	parent, name, err := parentFD(path)
	if err != nil {
		return result, err
	}
	defer func() { _ = syscall.Close(parent) }()
	fd, err := syscall.Openat(parent, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return result, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return result, err
	}
	if !before.Mode().IsRegular() || before.Sys().(*syscall.Stat_t).Nlink != 1 || before.Size() <= 0 || before.Size() > 16384 {
		return result, errors.New("unsafe receipt file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return result, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Sys().(*syscall.Stat_t).Nlink != 1 || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || len(raw) != int(before.Size()) {
		return result, errors.New("receipt changed during read")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("invalid receipt JSON")
	}
	return result, nil
}

func writeReceipt(path string, value receipt) error {
	parent, name, err := parentFD(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(parent) }()
	fd, err := syscall.Openat(parent, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return errors.New("unsafe new receipt leaf")
	}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) || !after.Mode().IsRegular() || after.Sys().(*syscall.Stat_t).Nlink != 1 || after.Size() <= 0 || after.Size() > 16384 {
		return errors.New("unsafe completed receipt")
	}
	return nil
}

func expectedSubject(nonce string) string {
	return subject.PositionFor("position-proof-"+nonce, "unrelated", "sentinel")
}

func sentinelProducerConfig(source, nonce string) bus.ProducerConfig {
	return bus.ProducerConfig{Source: producerSource, ProducerVersion: source, Tenant: "position-proof-" + nonce}
}

func sentinelEvent(nonce string, now time.Time) bus.Event {
	key := expectedSubject(nonce)
	return bus.Event{Subject: key, EventType: subject.PositionChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: now, PartitionKey: key, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: &domainpb.PositionState{PortfolioId: "unrelated", InstrumentId: "sentinel", Quantity: &commonpb.Decimal{Coefficient: 7}, AsOf: timestamppb.New(now)}}
}

func identity(value receipt) error {
	if value.Schema != 1 || !fullSHA.MatchString(value.Source) || !positiveID.MatchString(value.Run) || !positiveID.MatchString(value.Attempt) || value.Source != os.Getenv("GITHUB_SHA") || value.Run != os.Getenv("GITHUB_RUN_ID") || value.Attempt != os.Getenv("GITHUB_RUN_ATTEMPT") {
		return errors.New("source/run receipt identity differs")
	}
	if _, err := uuid.Parse(value.Nonce); err != nil {
		return errors.New("invalid sentinel owner nonce")
	}
	if value.SentinelSubject != expectedSubject(value.Nonce) || value.Phase != "seed" || value.SubjectCount != 1 || value.ServerID == "" || value.StreamCreated.IsZero() || value.MaxAge != 0 || value.MaxPerSubject != 1 || value.SentinelSequence == 0 {
		return errors.New("seed ownership/topology receipt differs")
	}
	if raw, err := hex.DecodeString(value.SentinelSHA256); err != nil || len(raw) != sha256.Size {
		return errors.New("invalid sentinel content identity")
	}
	return nil
}

func canonical(ctx context.Context, js jetstream.JetStream) (jetstream.Stream, *jetstream.StreamInfo, error) {
	s, err := js.Stream(ctx, "POSITION")
	if err != nil {
		return nil, nil, errors.New("canonical POSITION absent")
	}
	info, err := s.Info(ctx)
	if err != nil || info.Config.Name != "POSITION" || info.Config.MaxAge != 0 || info.Config.MaxMsgsPerSubject != 1 || len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != "risk.position.>" {
		return nil, nil, errors.New("canonical POSITION contract differs")
	}
	return s, info, nil
}

func run(phase, seed, output string) error {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return errors.New("CI fixture controller requires its named disposable job")
	}
	endpoint, err := url.Parse(os.Getenv("TEST_NATS_URL"))
	if err != nil || endpoint.Scheme != "nats" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Path != "" || (endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "localhost" && endpoint.Hostname() != "::1") || endpoint.Port() == "" {
		return errors.New("explicit loopback TEST_NATS_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nc, err := nats.Connect(endpoint.String(), nats.Timeout(5*time.Second))
	if err != nil {
		return errors.New("disposable broker connection refused")
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return errors.New("disposable JetStream unavailable")
	}
	stream, info, err := canonical(ctx, js)
	if err != nil {
		return err
	}
	value := receipt{Schema: 1, Phase: phase, Source: os.Getenv("GITHUB_SHA"), Run: os.Getenv("GITHUB_RUN_ID"), Attempt: os.Getenv("GITHUB_RUN_ATTEMPT"), ServerID: nc.ConnectedServerId(), ServerVersion: nc.ConnectedServerVersion(), StreamCreated: info.Created, MaxAge: int64(info.Config.MaxAge), MaxPerSubject: info.Config.MaxMsgsPerSubject, SubjectCount: info.State.NumSubjects}
	if !fullSHA.MatchString(value.Source) || !positiveID.MatchString(value.Run) || !positiveID.MatchString(value.Attempt) {
		return errors.New("missing full source/run identity")
	}
	if phase == "seed" {
		if value.SubjectCount != 0 {
			return fmt.Errorf("POSITION already contains %d subjects before the controller seed", value.SubjectCount)
		}
		value.Nonce = uuid.NewString()
		value.SentinelSubject = expectedSubject(value.Nonce)
		client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: endpoint.String(), Name: "position-reclaim-proof"})
		if err != nil {
			return errors.New("fixture producer transport unavailable")
		}
		defer func() { _ = client.Close() }()
		producer, err := bus.NewProducer(client, sentinelProducerConfig(value.Source, value.Nonce))
		if err != nil {
			return errors.New("canonical producer unavailable")
		}
		now := time.Now().UTC()
		err = producer.Publish(ctx, sentinelEvent(value.Nonce, now))
		if err != nil {
			return errors.New("canonical sentinel publication refused")
		}
	} else {
		previous, err := readReceipt(seed)
		if err != nil {
			return err
		}
		if err := identity(previous); err != nil {
			return err
		}
		if !value.StreamCreated.Equal(previous.StreamCreated) {
			return errors.New("canonical stream was replaced")
		}
		if value.ServerID != previous.ServerID || value.ServerVersion != previous.ServerVersion {
			return errors.New("broker or stream was replaced/restarted")
		}
		value.Nonce, value.SentinelSubject, value.SentinelSequence, value.SentinelSHA256 = previous.Nonce, previous.SentinelSubject, previous.SentinelSequence, previous.SentinelSHA256
	}
	message, err := stream.GetLastMsgForSubject(ctx, value.SentinelSubject)
	if err != nil {
		return errors.New("unrelated canonical sentinel missing")
	}
	env, _, err := bus.Unframe(message.Data)
	if err != nil || bus.Validate(env) != nil || env.GetSource() != producerSource || env.GetProducerVersion() != value.Source || env.GetTenantId() != "position-proof-"+value.Nonce {
		return errors.New("sentinel canonical frame/owner differs")
	}
	hash := sha256.Sum256(message.Data)
	actualHash := hex.EncodeToString(hash[:])
	if phase == "seed" {
		value.SentinelSequence = message.Sequence
		value.SentinelSHA256 = actualHash
	} else if value.SentinelSequence != message.Sequence || value.SentinelSHA256 != actualHash {
		return errors.New("unrelated sentinel bytes/sequence changed")
	}
	info, err = stream.Info(ctx)
	if err != nil {
		return errors.New("POSITION snapshot unavailable")
	}
	value.SubjectCount = info.State.NumSubjects
	if phase == "cleanup" {
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject(value.SentinelSubject)); err != nil {
			return errors.New("owned sentinel cleanup refused")
		}
		after, err := stream.Info(ctx)
		if err != nil || value.SubjectCount == 0 || after.State.NumSubjects != value.SubjectCount-1 {
			return errors.New("cleanup changed more than the exact owned sentinel")
		}
		value.CleanupAfterCount = &after.State.NumSubjects
	} else if value.SubjectCount != 1 {
		return fmt.Errorf("POSITION retains %d subjects; only the unrelated sentinel may remain", value.SubjectCount)
	}
	if err := writeReceipt(output, value); err != nil {
		return err
	}
	fmt.Printf("POSITION %s PASS: server/stream identity, exact sentinel and %d retained subjects\n", phase, value.SubjectCount)
	return nil
}

func main() {
	phase := flag.String("phase", "", "seed, snapshot or cleanup")
	seed := flag.String("seed", "", "protected seed receipt")
	output := flag.String("output", "", "new protected result receipt")
	flag.Parse()
	if (*phase != "seed" && *phase != "snapshot" && *phase != "cleanup") || *output == "" || (*phase != "seed" && *seed == "") {
		fmt.Fprintln(os.Stderr, "invalid fixture controller invocation")
		os.Exit(2)
	}
	if err := run(*phase, *seed, *output); err != nil {
		fmt.Fprintln(os.Stderr, "POSITION proof refused:", err)
		os.Exit(1)
	}
}
