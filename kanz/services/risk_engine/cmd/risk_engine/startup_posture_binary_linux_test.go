package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/services/risk_engine/internal/app"
)

var riskEngineBinary = flag.String("kanz-risk-engine-bin", "", "qualified public risk-engine executable")
var riskEngineBinarySHA256 = flag.String("kanz-risk-engine-bin-sha256", "", "SHA-256 of the qualified public risk-engine executable")

// The parent must build/promote this exact-source binary, bind it read-only and
// provide it through explicit public-artifact flags with an isolated canonical
// broker. Ordinary package tests omit these inputs;
// they do not establish #1064's actual-binary acceptance or OS confinement.
func TestRiskStartupPostureActualBinary(t *testing.T) {
	binary, wantHash := *riskEngineBinary, *riskEngineBinarySHA256
	if binary == "" && wantHash == "" {
		t.Skip("#1064 actual binary requires the qualified isolated fixture")
	}
	if !filepath.IsAbs(binary) || len(wantHash) != 64 {
		t.Fatal("qualified binary identity missing")
	}
	if _, err := hex.DecodeString(wantHash); err != nil {
		t.Fatal("invalid binary digest")
	}
	file, err := os.OpenFile(binary, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal("binary input unavailable")
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatal("binary metadata unavailable")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm()&0222 != 0 || stat.Nlink != 1 || before.Size() <= 0 || before.Size() > 128<<20 {
		_ = file.Close()
		t.Fatal("binary input custody refused")
	}
	hash := sha256.New()
	n, readErr := io.Copy(hash, io.LimitReader(file, (128<<20)+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || n != before.Size() || !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || hex.EncodeToString(hash.Sum(nil)) != wantHash {
		t.Fatal("binary identity changed or mismatched")
	}
	endpoint := os.Getenv("TEST_NATS_URL")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "nats" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
		t.Fatal("owned loopback canonical broker required")
	}
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "configured_idle"}[configured], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal("fixture address unavailable")
			}
			address := listener.Addr().String()
			if listener.Close() != nil {
				t.Fatal("fixture address release failed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			cmd := exec.CommandContext(ctx, binary)
			cmd.WaitDelay = 5 * time.Second
			cmd.Env = []string{"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "PATH=/nonexistent", "RISK_ENGINE_LISTEN=" + address, "RISK_ENGINE_SOURCE=risk-engine-posture", "RISK_ENGINE_TENANT=__system__"}
			if configured {
				cmd.Env = append(cmd.Env, "RISK_ENGINE_NATS_URL="+endpoint)
			}
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if cmd.Start() != nil {
				cancel()
				t.Fatal("risk binary start failed")
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			t.Cleanup(func() {
				defer cancel()
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Error("risk binary stop signal failed")
				}
				select {
				case err := <-done:
					if err != nil {
						t.Error("risk binary did not join cleanly")
					}
				case <-time.After(20 * time.Second):
					_ = cmd.Process.Kill()
					t.Error("risk binary shutdown deadline expired")
					select {
					case err := <-done:
						var exit *exec.ExitError
						if err != nil && !errors.As(err, &exit) {
							t.Error("forced child join failed")
						}
					case <-time.After(5 * time.Second):
						t.Error("risk binary forced join unconfirmed")
					}
				}
			})
			client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer client.CloseIdleConnections()
			deadline := time.Now().Add(15 * time.Second)
			for {
				families, err := riskStartupScrape(client, "http://"+address+"/metrics")
				if err == nil && riskStartupBinaryMetrics(families, configured) == nil && riskStartupReady(client, "http://"+address+"/readyz") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("actual binary startup metrics did not satisfy the fixed contract")
				}
				time.Sleep(25 * time.Millisecond)
			}
		})
	}
}

func riskStartupReady(client *http.Client, endpoint string) bool {
	response, err := client.Get(endpoint)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	n, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 257))
	return readErr == nil && n <= 256 && response.StatusCode == http.StatusOK
}

func riskStartupScrape(client *http.Client, endpoint string) (map[string]*dto.MetricFamily, error) {
	response, err := client.Get(endpoint)
	if err != nil {
		return nil, errors.New("scrape unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("scrape status refused")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("scrape bound refused")
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	return parser.TextToMetricFamilies(strings.NewReader(string(data)))
}

func riskStartupBinaryMetrics(families map[string]*dto.MetricFamily, configured bool) error {
	value := func(name string, labels map[string]string) (float64, error) {
		found := false
		result := 0.0
		for _, metric := range families[name].GetMetric() {
			if len(metric.Label) != len(labels) {
				continue
			}
			match := true
			for _, label := range metric.Label {
				want, exists := labels[label.GetName()]
				match = match && exists && want == label.GetValue()
			}
			if !match {
				continue
			}
			if found {
				return 0, fmt.Errorf("duplicate startup scalar %s", name)
			}
			found = true
			if metric.Gauge != nil {
				result = metric.Gauge.GetValue()
			} else if metric.Counter != nil {
				result = metric.Counter.GetValue()
			} else {
				return 0, errors.New("startup scalar type refused")
			}
		}
		if !found {
			return 0, fmt.Errorf("missing startup scalar %s", name)
		}
		return result, nil
	}
	wantConfigured := 0.0
	if configured {
		wantConfigured = 1
	}
	for _, name := range []string{"kanz_bus_configured", "kanz_bus_connected"} {
		got, err := value(name, map[string]string{"client": "risk-engine-posture"})
		if err != nil || got != wantConfigured {
			return errors.New("broker startup posture mismatch")
		}
	}
	ready, err := value("kanz_risk_measure_registry_ready", map[string]string{})
	if err != nil || ready != wantConfigured {
		return errors.New("registry startup posture mismatch")
	}
	for _, event := range []string{"disconnected", "reconnected", "closed"} {
		got, err := value("kanz_bus_connection_events_total", map[string]string{"client": "risk-engine-posture", "event": event})
		if err != nil || got != 0 {
			return errors.New("startup connection counter mismatch")
		}
	}
	for _, status := range []string{"ok", "error"} {
		got, err := value("kanz_risk_recompute_total", map[string]string{"status": status})
		if err != nil || got != 0 {
			return errors.New("idle recompute counter mismatch")
		}
	}
	served := map[string]bool{}
	if configured {
		for _, name := range compute.DefaultRegistry().Names() {
			served[string(name)] = true
		}
	}
	for _, measure := range compute.Catalogue() {
		name := string(measure.Name)
		want := 0.0
		if served[name] {
			want = 1
		}
		live, err := value("kanz_risk_measure_live", map[string]string{"measure": name, "family": string(measure.Family)})
		if err != nil || live != want {
			return errors.New("registry identity mismatch")
		}
		method := app.MethodUnconfigured
		if configured && served[name] {
			method = app.MethodUnobserved
		}
		methodValue, methodErr := value("kanz_risk_measure_method", map[string]string{"measure": name, "method": method})
		if !configured && (methodErr != nil || methodValue != 1) || configured && served[name] && (methodErr != nil || methodValue != 1) {
			return errors.New("startup method posture mismatch")
		}
		if configured {
			unconfigured, err := value("kanz_risk_measure_method", map[string]string{"measure": name, "method": app.MethodUnconfigured})
			if err != nil || unconfigured != 0 {
				return errors.New("final registry retained unconfigured method state")
			}
		}
	}
	if !configured {
		for _, name := range []string{"kanz_bus_pending_messages", "kanz_bus_consumer_lag", "kanz_bus_backlog_poll_last_success_timestamp_seconds"} {
			if len(families[name].GetMetric()) != 0 {
				return errors.New("unconfigured broker fabricated backlog")
			}
		}
	}
	return nil
}
