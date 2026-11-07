//go:build linux && amd64

package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/internal/topic"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/lake-sink/internal/sink"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
)

const snapshotImageMemory = int64(1536 << 20)

// Docker is a qualification dependency, never a runtime owner dependency.
// The dedicated job supplies immutable IDs from its actual source-bound builds.
func TestSnapshotActualHybridImageEntrypointAndAutomaticLifecycleLanding(t *testing.T) {
	image, workerHash, redpanda := os.Getenv("TEST_LAKE_IMAGE_ID"), os.Getenv("TEST_LAKE_IMAGE_WORKER_SHA256"), os.Getenv("TEST_LAKE_REDPANDA_IMAGE_ID")
	if image == "" && workerHash == "" && redpanda == "" {
		t.Skip("dedicated actual hybrid image qualification required")
	}
	for _, id := range []string{image, redpanda} {
		if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(id) {
			t.Fatal("actual immutable image IDs required")
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(workerHash) {
		t.Fatal("actual image worker digest required")
	}
	version := os.Getenv("TEST_LAKE_IMAGE_VERSION")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(version) {
		t.Fatal("actual image source version required")
	}
	for _, key := range []string{"TEST_POSTGRES_URL", "TEST_LAKE_NATS_SERVER", "TEST_LAKE_NATS_VERSION", "TEST_LAKE_GATEWAY_TEST_BIN", "TEST_LAKE_ARCHIVER_TEST_BIN", "TEST_LAKE_CLI_BIN", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT"} {
		if os.Getenv(key) == "" {
			t.Fatal("enabled actual image proof requires every dependency")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	a, _ := snapshotStores(t)
	b := newSnapshotBroker(t)
	owned := newSnapshotImageOwner(t)
	plain, tlsBroker := owned.redpanda(t, ctx, redpanda, b)
	admin, err := b.connect(b.adminA)
	if err != nil {
		t.Fatal("actual broker administrator unavailable")
	}
	defer admin.Close()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal("actual JetStream unavailable")
	}
	if _, err := js.CreateStream(ctx, snapshotprofile.Stream()); err != nil {
		t.Fatal("actual lifecycle stream unavailable")
	}
	provisionSnapshotRegisteredDependencies(t, ctx, js)
	dataTopic := "tenant-a.market.trade"
	lifecycleTopic := topic.Qualify("tenant-a", "lake.snapshot")
	client := &kafka.Client{Addr: kafka.TCP(plain), Timeout: 5 * time.Second}
	created, err := client.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{
		{Topic: dataTopic, NumPartitions: 1, ReplicationFactor: 1},
		{Topic: lifecycleTopic, NumPartitions: 1, ReplicationFactor: 1},
		{Topic: "dlq.archiver", NumPartitions: 1, ReplicationFactor: 1},
	}})
	if err != nil || len(created.Errors) != 3 {
		t.Fatal("actual owned Kafka topics unavailable")
	}
	for _, err := range created.Errors {
		if err != nil {
			t.Fatal("actual Kafka topic creation refused")
		}
	}
	archiver := startActualSnapshotArchiver(t, ctx, b, plain, "tenant-a")
	defer archiver.close(t)
	wapi, socket := testidentity.StartWorkload(t, b.source)
	socketPath := strings.TrimPrefix(socket, "unix://")
	// Bind only this synthetic socket inode; its host parent remains private.
	if os.Chmod(socketPath, 0o666) != nil {
		t.Fatal("synthetic image workload socket unavailable")
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if os.WriteFile(policy, []byte(`{"roles":{"creator":["lake.snapshot.create"],"reader":["lake.snapshot.read"]}}`), 0o444) != nil {
		t.Fatal("synthetic image policy unavailable")
	}
	grpcAddress, healthAddress := snapshotImageAddress(t), snapshotImageAddress(t)
	application := "lake-image-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	dsn, err := snapshotFixtureDSN(a.pool.Config().ConnConfig.ConnString(), a.pool.Config().ConnConfig.RuntimeParams["search_path"], application)
	if err != nil {
		t.Fatal("scoped image database URL unavailable")
	}
	volume := owned.volume(t, ctx)
	environment := map[string]string{
		"SPIFFE_ENDPOINT_SOCKET": "unix:///proof/workload.sock", "LAKE_SINK_RETAIN_RAW_FRAME": "true",
		"LAKE_SINK_BROKERS": tlsBroker, "LAKE_SINK_TOPICS": dataTopic, // no explicit lifecycle topic
		"LAKE_SINK_OUTPUT_DIR": "/tmp/landing", "LAKE_SINK_LISTEN": healthAddress,
		"LAKE_SINK_CONSUMER_GROUP": "image-" + owned.label,
		"LAKE_SNAPSHOT_ENABLED":    "true", "LAKE_SNAPSHOT_TENANT": "tenant-a", "LAKE_SNAPSHOT_LISTEN": grpcAddress,
		"LAKE_SNAPSHOT_OUTPUT_DIR": "/tmp", "LAKE_SNAPSHOT_DATABASE_URL": dsn,
		"LAKE_SNAPSHOT_NATS_URL": "tls://" + b.address, "LAKE_SNAPSHOT_NATS_ACCOUNT": "acme",
		"LAKE_SNAPSHOT_POLICY_FILE": "/proof/policy.json", "LAKE_SNAPSHOT_WORKER_PATH": "/kanz-lake-snapshot",
		"LAKE_SNAPSHOT_WORKER_SHA256": workerHash, "LAKE_SNAPSHOT_GATEWAY_ID": "spiffe://kanz.internal/ns/kanz-services/sa/api-gateway",
	}
	args := []string{"--network", "host", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--memory", strconv.FormatInt(snapshotImageMemory, 10), "--memory-swap", strconv.FormatInt(snapshotImageMemory, 10), "--pids-limit", "64",
		"--mount", "type=volume,src=" + volume + ",dst=/tmp",
		"--mount", "type=bind,src=" + socketPath + ",dst=/proof/workload.sock,readonly",
		"--mount", "type=bind,src=" + policy + ",dst=/proof/policy.json,readonly"}
	for key, value := range environment {
		args = append(args, "--env", key+"="+value)
	}
	container := owned.start(t, ctx, image, args)
	imageWait(t, ctx, func() bool {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+healthAddress+"/readyz", nil)
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK && wapi.Active() > 0
	})
	row := custodyRow(t, "tenant-a")
	writer := &kafka.Writer{Addr: kafka.TCP(plain), Topic: dataTopic, RequiredAcks: kafka.RequireAll, WriteTimeout: 5 * time.Second}
	defer writer.Close()
	var messages []kafka.Message
	for _, id := range []string{"image-first", "image-second"} {
		var frame envelopepb.EventFrame
		if proto.Unmarshal(row.RawCapture.Frame, &frame) != nil {
			t.Fatal("canonical fixture frame unavailable")
		}
		frame.Envelope.EventId, frame.Envelope.IdempotencyKey = id, id
		wire, err := proto.Marshal(&frame)
		if err != nil {
			t.Fatal("canonical fixture encoding failed")
		}
		wire = append(wire, 0x0a, 0x00, 0xa0, 0x06, 0x81, 0x00)
		var decoded envelopepb.EventFrame
		if proto.Unmarshal(wire, &decoded) != nil {
			t.Fatal("original-wire fixture invalid")
		}
		normalized, err := proto.Marshal(&decoded)
		if err != nil || bytes.Equal(normalized, wire) {
			t.Fatal("image custody fixture does not distinguish remarshal")
		}
		messages = append(messages, kafka.Message{Key: []byte(id), Value: wire})
	}
	if writer.WriteMessages(ctx, messages...) != nil {
		t.Fatal("actual Kafka input publication failed")
	}
	imageWait(t, ctx, func() bool { return len(owned.rows(t, ctx, container, "market.trade")) == 2 })
	runActualGatewayAndCLI(t, ctx, b, grpcAddress, row)
	// Creation/publishing facts must traverse the existing real archiver and the
	// main entrypoint's automatically tenant-qualified Kafka subscription.
	var published []sink.Row
	imageWait(t, ctx, func() bool {
		published = owned.rows(t, ctx, container, "lake.snapshot.published")
		return len(published) > 0
	})
	for _, landed := range published {
		var frame envelopepb.EventFrame
		if landed.RawCapture == nil || proto.Unmarshal(landed.RawCapture.Frame, &frame) != nil || frame.Envelope == nil || frame.Envelope.ProducerVersion != version || frame.Envelope.Source != Service || frame.Envelope.TenantId != "tenant-a" {
			t.Fatal("actual image lifecycle producer source binding differs")
		}
	}
	rows := owned.rows(t, ctx, container, "market.trade")
	if len(rows) != 2 {
		t.Fatal("actual image landing count changed")
	}
	for i, landed := range rows {
		if landed.RawCapture == nil || !bytes.Equal(landed.RawCapture.Frame, messages[i].Value) || landed.RawCapture.Kafka == nil || landed.RawCapture.Kafka.Topic != dataTopic {
			t.Fatal("actual image lost original Kafka custody")
		}
	}
	owned.proveRuntime(t, ctx, container, image)
	owned.docker(t, ctx, "stop", "--time", "30", container)
	state := strings.TrimSpace(string(owned.docker(t, ctx, "inspect", "--format", "{{.State.ExitCode}}", container)))
	if state != "0" {
		t.Fatal("actual entrypoint did not join cleanly")
	}
	imageWait(t, ctx, func() bool { return wapi.Active() == 0 })
	var sessions int
	if a.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, application).Scan(&sessions) != nil || sessions != 0 {
		t.Fatal("actual image PostgreSQL sessions remained after exit")
	}
	t.Log("actual hybrid image: immutable image/worker; nonroot read-only root; WAPI/mTLS; canonical archive automatic landing; authenticated gateway/CLI; joined entrypoint")
	t.Logf("actual hybrid identity: image=%s worker=%s source=%s", image, workerHash, version)
}

func snapshotImageAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("disposable loopback port unavailable")
	}
	address := l.Addr().String()
	if l.Close() != nil {
		t.Fatal("disposable loopback listener did not close")
	}
	return address
}

func imageWait(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("actual image observation deadline")
		case <-timer.C:
		}
	}
}

type snapshotImageOwner struct {
	label      string
	run        string
	containers []string
	volumes    []string
}

func newSnapshotImageOwner(t *testing.T) *snapshotImageOwner {
	t.Helper()
	run, attempt := os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT")
	for _, value := range []string{run, attempt} {
		if !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(value) {
			t.Fatal("actual image proof requires source-bound run/attempt")
		}
	}
	o := &snapshotImageOwner{label: uuid.NewString(), run: run + "-" + attempt}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for i := len(o.containers) - 1; i >= 0; i-- {
			id := o.containers[i]
			found, err := o.dockerResult(ctx, "ps", "-a", "--filter", "name=^/"+id+"$", "--format", "{{.Names}}")
			if err == nil && len(bytes.TrimSpace(found)) == 0 {
				continue
			}
			raw, inspectErr := o.dockerResult(ctx, "inspect", "--format", `{{index .Config.Labels "kanz.snapshot-image"}}|{{index .Config.Labels "kanz.snapshot-image-run"}}`, id)
			if err != nil || strings.TrimSpace(string(found)) != id || inspectErr != nil || strings.TrimSpace(string(raw)) != o.label+"|"+o.run {
				t.Error("container cleanup ownership mismatch")
				continue
			}
			if _, err := o.dockerResult(ctx, "rm", "-f", "-v", id); err != nil {
				t.Error("owned container removal failed")
			}
		}
		for _, name := range o.volumes {
			found, err := o.dockerResult(ctx, "volume", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
			if err == nil && len(bytes.TrimSpace(found)) == 0 {
				continue
			}
			raw, inspectErr := o.dockerResult(ctx, "volume", "inspect", "--format", `{{index .Labels "kanz.snapshot-image"}}|{{index .Labels "kanz.snapshot-image-run"}}`, name)
			if err != nil || strings.TrimSpace(string(found)) != name || inspectErr != nil || strings.TrimSpace(string(raw)) != o.label+"|"+o.run {
				t.Error("volume cleanup ownership mismatch")
				continue
			}
			if _, err := o.dockerResult(ctx, "volume", "rm", name); err != nil {
				t.Error("owned volume removal failed")
			}
		}
		for _, kind := range []string{"ps", "volume"} {
			args := []string{"ps", "-a"}
			if kind == "volume" {
				args = []string{"volume", "ls"}
			}
			format := "{{.ID}}"
			if kind == "volume" {
				format = "{{.Name}}"
			}
			args = append(args, "--filter", "label=kanz.snapshot-image="+o.label, "--format", format)
			raw, err := o.dockerResult(ctx, args...)
			if err != nil || len(bytes.TrimSpace(raw)) != 0 {
				t.Error("owned image fixture resources remain")
			}
		}
	})
	return o
}

func (o *snapshotImageOwner) docker(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	out, err := o.dockerResult(ctx, args...)
	if err != nil {
		t.Fatal("owned Docker operation failed") // never emit argv/DSN/config
	}
	return out
}

func (o *snapshotImageOwner) dockerResult(ctx context.Context, args ...string) ([]byte, error) {
	call, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(call, "docker", args...)
	var out snapshotProcessOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = 5 * time.Second
	if cmd.Run() != nil {
		return nil, errors.New("owned Docker operation failed")
	}
	return out.Bytes(), nil
}

func (o *snapshotImageOwner) volume(t *testing.T, ctx context.Context) string {
	t.Helper()
	name := fmt.Sprintf("kanz-lake-image-%s-%d", o.label, len(o.volumes))
	if len(bytes.TrimSpace(o.docker(t, ctx, "volume", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}"))) != 0 {
		t.Fatal("disposable image volume already exists")
	}
	// Record the exclusive creation intent before Docker can create anything;
	// cleanup reconciles an ambiguous reply by exact name and both labels.
	o.volumes = append(o.volumes, name)
	if strings.TrimSpace(string(o.docker(t, ctx, "volume", "create", "--label", "kanz.snapshot-image="+o.label, "--label", "kanz.snapshot-image-run="+o.run, name))) != name {
		t.Fatal("disposable image volume identity differs")
	}
	if strings.TrimSpace(string(o.docker(t, ctx, "volume", "inspect", "--format", `{{index .Labels "kanz.snapshot-image"}}|{{index .Labels "kanz.snapshot-image-run"}}`, name))) != o.label+"|"+o.run {
		t.Fatal("created volume ownership differs")
	}
	return name
}

func (o *snapshotImageOwner) start(t *testing.T, ctx context.Context, image string, args []string, entryArgs ...string) string {
	t.Helper()
	name := fmt.Sprintf("kanz-lake-image-%s-%d", o.label, len(o.containers))
	if len(bytes.TrimSpace(o.docker(t, ctx, "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}"))) != 0 {
		t.Fatal("disposable image container already exists")
	}
	o.containers = append(o.containers, name)
	command := []string{"create", "--name", name, "--label", "kanz.snapshot-image=" + o.label, "--label", "kanz.snapshot-image-run=" + o.run}
	command = append(command, args...)
	command = append(command, image)
	command = append(command, entryArgs...)
	id := strings.TrimSpace(string(o.docker(t, ctx, command...)))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Fatal("invalid owned container identity")
	}
	if strings.TrimSpace(string(o.docker(t, ctx, "inspect", "--format", `{{.Id}}|{{index .Config.Labels "kanz.snapshot-image"}}|{{index .Config.Labels "kanz.snapshot-image-run"}}`, name))) != id+"|"+o.label+"|"+o.run {
		t.Fatal("created container ownership differs")
	}
	o.docker(t, ctx, "start", id)
	return id
}

func (o *snapshotImageOwner) redpanda(t *testing.T, ctx context.Context, image string, b *snapshotBroker) (string, string) {
	t.Helper()
	plain, tlsAddress, rpc, admin := snapshotImageAddress(t), snapshotImageAddress(t), snapshotImageAddress(t), snapshotImageAddress(t)
	_, adminPort, _ := net.SplitHostPort(admin)
	port, err := strconv.Atoi(adminPort)
	if err != nil {
		t.Fatal("disposable broker admin address invalid")
	}
	args := []string{"--network", "host", "--memory", "2147483648", "--memory-swap", "2147483648", "--pids-limit", "128",
		"--mount", "type=volume,src=" + o.volume(t, ctx) + ",dst=/var/lib/redpanda/data"}
	for source, target := range map[string]string{"cert.pem": "cert.pem", "key.pem": "key.pem", "ca.pem": "ca.pem"} {
		path := filepath.Join(b.root, source)
		// Only a synthetic test key, bound as one file from a private host directory.
		if os.Chmod(path, 0o444) != nil {
			t.Fatal("synthetic broker TLS fixture unavailable")
		}
		args = append(args, "--mount", "type=bind,src="+path+",dst=/proof/"+target+",readonly")
	}
	tlsConfig, err := json.Marshal([]map[string]any{{"name": "tls", "enabled": true, "require_client_auth": true,
		"cert_file": "/proof/cert.pem", "key_file": "/proof/key.pem", "truststore_file": "/proof/ca.pem"}})
	if err != nil {
		t.Fatal("synthetic broker TLS configuration invalid")
	}
	adminConfig, err := json.Marshal([]map[string]any{{"address": "127.0.0.1", "port": port}})
	if err != nil {
		t.Fatal("synthetic broker admin configuration invalid")
	}
	o.start(t, ctx, image, args, "redpanda", "start", "--smp=1", "--memory=1G", "--reserve-memory=0M", "--overprovisioned", "--check=false", "--unsafe-bypass-fsync=false", "--node-id=0",
		"--kafka-addr=plain://"+plain+",tls://"+tlsAddress, "--advertise-kafka-addr=plain://"+plain+",tls://"+tlsAddress,
		"--rpc-addr="+rpc, "--advertise-rpc-addr="+rpc,
		"--set", "redpanda.admin="+string(adminConfig), "--set", "redpanda.kafka_api_tls="+string(tlsConfig),
		"--set", "redpanda.auto_create_topics_enabled=false", "--set", "redpanda.default_topic_replications=1", "--set", "redpanda.write_caching_default=false")
	client := &kafka.Client{Addr: kafka.TCP(plain), Timeout: time.Second}
	imageWait(t, ctx, func() bool { _, err := client.Metadata(ctx, &kafka.MetadataRequest{}); return err == nil })
	tlsTransport := &kafka.Transport{TLS: transport.ClientTLSConfig(b.source, transport.AuthorizeMesh()), DialTimeout: time.Second}
	defer tlsTransport.CloseIdleConnections()
	tlsClient := &kafka.Client{Addr: kafka.TCP(tlsAddress), Transport: tlsTransport, Timeout: 3 * time.Second}
	if _, err := tlsClient.Metadata(ctx, &kafka.MetadataRequest{}); err != nil {
		t.Fatal("actual mTLS Kafka positive control failed")
	}
	// Preserve broker verification while withholding all client credentials.
	// Plaintext refusal alone cannot prove mandatory client authentication.
	noClientTLS := tlsTransport.TLS.Clone()
	noClientTLS.Certificates, noClientTLS.GetClientCertificate = nil, nil
	noClientTransport := &kafka.Transport{TLS: noClientTLS, DialTimeout: time.Second}
	defer noClientTransport.CloseIdleConnections()
	noClient := &kafka.Client{Addr: kafka.TCP(tlsAddress), Transport: noClientTransport, Timeout: 3 * time.Second}
	if _, err := noClient.Metadata(ctx, &kafka.MetadataRequest{}); err == nil {
		t.Fatal("Kafka TLS listener accepted no client certificate")
	}
	if _, err := tlsClient.Metadata(ctx, &kafka.MetadataRequest{}); err != nil {
		t.Fatal("authenticated Kafka control failed after client-auth refusal")
	}
	plainClient := &kafka.Client{Addr: kafka.TCP(tlsAddress), Timeout: time.Second}
	if _, err := plainClient.Metadata(ctx, &kafka.MetadataRequest{}); err == nil {
		t.Fatal("Kafka TLS listener accepted plaintext")
	}
	return plain, tlsAddress
}

func (o *snapshotImageOwner) rows(t *testing.T, ctx context.Context, id, eventType string) []sink.Row {
	t.Helper()
	wire := o.docker(t, ctx, "cp", id+":/tmp/landing/.", "-")
	reader := tar.NewReader(bytes.NewReader(wire))
	var rows []sink.Row
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || header.Size > 1<<20 {
			t.Fatal("bounded actual landing archive invalid")
		}
		if !header.FileInfo().Mode().IsRegular() || !strings.HasSuffix(header.Name, ".ndjson") {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(reader, 1<<20))
		if err != nil {
			t.Fatal("actual landing unreadable")
		}
		lines := bytes.Split(body, []byte{'\n'})
		// Docker observes an appendable file independently of the writer lock;
		// only complete newline-terminated rows can satisfy this observation.
		for _, line := range lines[:len(lines)-1] {
			if len(line) == 0 {
				continue
			}
			var row sink.Row
			if json.Unmarshal(line, &row) != nil {
				t.Fatal("actual landing row malformed")
			}
			if row.EventType == eventType {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

func (o *snapshotImageOwner) proveRuntime(t *testing.T, ctx context.Context, id, image string) {
	t.Helper()
	var state struct {
		PID       int    `json:"pid"`
		ReadOnly  bool   `json:"readonly"`
		Memory    int64  `json:"memory"`
		PidsLimit int64  `json:"pids"`
		Image     string `json:"image"`
	}
	projection := `{"pid":{{.State.Pid}},"readonly":{{.HostConfig.ReadonlyRootfs}},"memory":{{.HostConfig.Memory}},"pids":{{.HostConfig.PidsLimit}},"image":{{json .Image}}}`
	if json.Unmarshal(o.docker(t, ctx, "inspect", "--format", projection, id), &state) != nil || state.PID <= 0 || !state.ReadOnly || state.Memory != snapshotImageMemory || state.PidsLimit != 64 || state.Image != image {
		t.Fatal("actual image resource/readonly contract differs")
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", state.PID))
	if err != nil || len(status) > 64<<10 || !bytes.Contains(status, []byte("Uid:\t65532\t65532\t65532\t65532")) || !bytes.Contains(status, []byte("NoNewPrivs:\t1")) {
		t.Fatal("actual image principal or no-new-privileges differs")
	}
	group, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", state.PID))
	if err != nil || len(group) > 4096 || !bytes.HasPrefix(group, []byte("0::/")) || bytes.Count(group, []byte{'\n'}) != 1 {
		t.Fatal("actual cgroup-v2 identity unavailable")
	}
	relative := strings.TrimSpace(strings.TrimPrefix(string(group), "0::"))
	if filepath.Clean(relative) != relative || !strings.Contains(relative, id) {
		t.Fatal("actual cgroup ownership differs")
	}
	base := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(relative, "/"))
	values := map[string]int64{}
	for _, name := range []string{"memory.max", "memory.peak", "pids.max"} {
		body, err := os.ReadFile(filepath.Join(base, name))
		value, parseErr := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
		if err != nil || parseErr != nil || value <= 0 {
			t.Fatal("actual cgroup numeric observation unavailable")
		}
		values[name] = value
	}
	if values["memory.max"] != snapshotImageMemory || values["memory.peak"] > snapshotImageMemory || values["pids.max"] != 64 {
		t.Fatal("actual cgroup limits differ")
	}
	t.Logf("actual hybrid cgroup: memory_max=%d memory_peak=%d pids_max=%d", values["memory.max"], values["memory.peak"], values["pids.max"])
}
