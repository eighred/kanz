package config

import (
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eighred/kanz/pkg/secret"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Snapshot is absent by default. Enabling the finite tenant-wide owner requires
// every dependency and identity explicitly; none is inherited from a caller.
type Snapshot struct {
	Tenant       string
	Listen       string
	OutputDir    string
	DatabaseURL  string
	NATSURL      string
	NATSAccount  string
	PolicyFile   string
	WorkerPath   string
	WorkerSHA256 [32]byte
	GatewayID    spiffeid.ID
}

func loadSnapshot(base Config) (*Snapshot, error) {
	enabled := false
	if value, present := os.LookupEnv("LAKE_SNAPSHOT_ENABLED"); present {
		var err error
		enabled, err = strconv.ParseBool(value)
		if err != nil {
			return nil, errors.New("LAKE_SNAPSHOT_ENABLED must be a boolean")
		}
	}
	if !enabled {
		return nil, nil
	}
	if !base.RetainRawFrame || base.SPIFFESocket == "" {
		return nil, errors.New("snapshot requires raw custody and workload identity")
	}
	cfg := &Snapshot{Tenant: os.Getenv("LAKE_SNAPSHOT_TENANT"), Listen: os.Getenv("LAKE_SNAPSHOT_LISTEN"), OutputDir: os.Getenv("LAKE_SNAPSHOT_OUTPUT_DIR"), NATSURL: os.Getenv("LAKE_SNAPSHOT_NATS_URL"), NATSAccount: os.Getenv("LAKE_SNAPSHOT_NATS_ACCOUNT"), PolicyFile: os.Getenv("LAKE_SNAPSHOT_POLICY_FILE"), WorkerPath: os.Getenv("LAKE_SNAPSHOT_WORKER_PATH")}
	if cfg.NATSAccount == "" || len(cfg.NATSAccount) > 256 || !utf8.ValidString(cfg.NATSAccount) || strings.TrimSpace(cfg.NATSAccount) != cfg.NATSAccount || strings.ContainsAny(cfg.NATSAccount, "\x00\r\n") {
		return nil, errors.New("snapshot requires the exact explicit NATS account key")
	}
	if cfg.Tenant == "" || len(cfg.Tenant) > 256 || !utf8.ValidString(cfg.Tenant) || strings.TrimSpace(cfg.Tenant) != cfg.Tenant || strings.ContainsAny(cfg.Tenant, "\x00\r\n") {
		return nil, errors.New("snapshot requires an explicit bounded tenant")
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return nil, errors.New("snapshot requires an explicit gRPC listen address")
	}
	if cfg.NATSURL == "" || cfg.PolicyFile == "" || !filepath.IsAbs(base.OutputDir) || !filepath.IsAbs(cfg.OutputDir) || !filepath.IsAbs(cfg.WorkerPath) {
		return nil, errors.New("snapshot requires explicit broker, policy and absolute custody paths")
	}
	cfg.DatabaseURL, err = secret.Read("LAKE_SNAPSHOT_DATABASE_URL")
	if err != nil || cfg.DatabaseURL == "" {
		return nil, errors.New("snapshot database configuration unavailable")
	}
	sha, err := hex.DecodeString(os.Getenv("LAKE_SNAPSHOT_WORKER_SHA256"))
	if err != nil || len(sha) != 32 {
		return nil, errors.New("snapshot requires exact worker SHA256")
	}
	copy(cfg.WorkerSHA256[:], sha)
	if cfg.WorkerSHA256 == ([32]byte{}) {
		return nil, errors.New("snapshot worker SHA256 is empty")
	}
	cfg.GatewayID, err = spiffeid.FromString(os.Getenv("LAKE_SNAPSHOT_GATEWAY_ID"))
	if err != nil || cfg.GatewayID.Path() != "/ns/kanz-services/sa/api-gateway" {
		return nil, errors.New("snapshot requires the exact gateway workload identity")
	}
	return cfg, nil
}
