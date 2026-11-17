package riskdedupproof

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestFixtureBroker(t *testing.T) {
	if os.Getenv("KANZ_RISK_DEDUP_PROOF_PHASE") != "broker" {
		t.Skip("requires the disposable broker namespace")
	}
	if err := runFixtureBroker(); err != nil {
		t.Fatalf("local broker fixture refused: %v", err)
	}
}

func runFixtureBroker() error {
	if server.VERSION != "2.15.3" {
		return fmt.Errorf("unexpected broker version")
	}
	opts, err := server.ProcessConfigFile("/inputs/nats-dev.conf")
	if err != nil {
		return err
	}
	opts.Host, opts.Port, opts.StoreDir = "127.0.0.1", 4222, "/tmp/nats-data"
	opts.NoLog, opts.NoSigs = true, true
	s, err := server.NewServer(opts)
	if err != nil {
		return err
	}
	s.Start()
	defer func() { s.Shutdown(); s.WaitForShutdown() }()
	if !s.ReadyForConnections(5 * time.Second) {
		return fmt.Errorf("not ready")
	}
	nc, err := nats.Connect("nats://127.0.0.1:4222", nats.Timeout(2*time.Second))
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(3 * time.Second))
	if err != nil {
		return err
	}
	input, err := os.Open("/inputs/streams.yaml")
	if err != nil {
		return err
	}
	defer input.Close()
	declaration := regexp.MustCompile(`^\s*ensure_stream\s+(\w+)\s+"([^"]+)"\s+(\S+)(.*)$`)
	count := 0
	scan := bufio.NewScanner(input)
	for scan.Scan() {
		m := declaration.FindStringSubmatch(scan.Text())
		if m == nil {
			continue
		}
		age, err := time.ParseDuration(m[3])
		if err != nil {
			return err
		}
		// Bounded disposable topology derived from the source declarations.
		// It is not a PostSync/CLI/GitOps execution or full topology acceptance.
		cfg := &nats.StreamConfig{Name: m[1], Subjects: strings.Split(m[2], ","),
			Storage: nats.FileStorage, Retention: nats.LimitsPolicy, Replicas: 1,
			MaxAge: age, Duplicates: 2 * time.Minute, MaxBytes: 8 << 20,
			MaxMsgs: -1, MaxConsumers: -1, MaxMsgSize: -1, MaxMsgsPerSubject: -1}
		for _, flag := range strings.Fields(m[4]) {
			switch {
			case flag == "--discard=new":
				cfg.Discard = nats.DiscardNew
			case strings.HasPrefix(flag, "--max-msgs-per-subject="):
				cfg.MaxMsgsPerSubject, err = strconv.ParseInt(strings.TrimPrefix(flag, "--max-msgs-per-subject="), 10, 64)
				if err != nil {
					return err
				}
			case flag == "--max-msgs=-1" || flag == "--max-bytes=-1":
				// Keep this fixture's explicit resource bound.
			default:
				return fmt.Errorf("unsupported source fixture flag")
			}
		}
		if _, err := js.AddStream(cfg); err != nil {
			return err
		}
		count++
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if count != 18 {
		return fmt.Errorf("source stream declaration count changed")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	fmt.Println("LOCAL_NATS_READY_2.15.3")
	<-stop
	return nil
}
