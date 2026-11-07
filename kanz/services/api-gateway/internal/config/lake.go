package config

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

type LakeSnapshot struct {
	Address    string
	Tenant     string
	Peer       spiffeid.ID
	CreateRole string
	ReadRole   string
}

func loadLakeSnapshot(socket string) (*LakeSnapshot, error) {
	values := []string{os.Getenv("API_GATEWAY_LAKE_SNAPSHOT_ADDR"), os.Getenv("API_GATEWAY_LAKE_SNAPSHOT_TENANT"), os.Getenv("API_GATEWAY_LAKE_SNAPSHOT_PEER_ID"), os.Getenv("API_GATEWAY_LAKE_SNAPSHOT_CREATE_ROLE"), os.Getenv("API_GATEWAY_LAKE_SNAPSHOT_READ_ROLE")}
	enabled := false
	for _, value := range values {
		enabled = enabled || value != ""
	}
	if !enabled {
		return nil, nil
	}
	for _, value := range values {
		if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("lake snapshot gateway requires complete bounded configuration")
		}
	}
	host, port, err := net.SplitHostPort(values[0])
	p, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || p < 1 || p > 65535 || socket == "" {
		return nil, errors.New("lake snapshot gateway requires an explicit TCP target and workload identity")
	}
	peer, err := spiffeid.FromString(values[2])
	if err != nil || peer.Path() != "/ns/kanz-services/sa/lake-sink" {
		return nil, errors.New("lake snapshot gateway requires exact lake workload identity")
	}
	if len(values[3]) > 128 || len(values[4]) > 128 {
		return nil, errors.New("lake snapshot role exceeds bound")
	}
	return &LakeSnapshot{Address: values[0], Tenant: values[1], Peer: peer, CreateRole: values[3], ReadRole: values[4]}, nil
}
