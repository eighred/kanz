// kanz-lake-snapshot is an authenticated operator client for the optional finite
// snapshot owner. It never opens landing/Parquet files or contacts the worker,
// database, broker, or a venue. All authority is decided by api-gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	gatewayclient "github.com/eighred/kanz/internal/tui/gateway"
	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("kanz-lake-snapshot", flag.ContinueOnError)
	endpoint := fs.String("gateway", os.Getenv("KANZ_GATEWAY_URL"), "authenticated gateway HTTPS URL")
	operation := fs.String("operation", "", "create, get, list, count, or failures")
	requestID := fs.String("request-id", "", "stable create request ID; reuse unchanged after uncertain response")
	snapshotID := fs.String("snapshot-id", "", "opaque registered snapshot UUID")
	from := fs.String("from", "", "inclusive event timestamp (RFC3339Nano)")
	until := fs.String("until", "", "exclusive event timestamp (RFC3339Nano)")
	types := fs.String("event-types", "", "comma-separated exact event types")
	page := fs.Uint("page-size", 100, "finite page/group bound, 1..1000")
	after := fs.Uint64("after-ordinal", 0, "exclusive selected-row ordinal; omit for first page")
	if err := fs.Parse(args); err != nil {
		return errors.New("invalid snapshot command flags")
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected snapshot command argument")
	}
	u, err := url.Parse(*endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid gateway endpoint")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()) {
		return errors.New("gateway must use HTTPS (literal loopback HTTP is test-only)")
	}
	var afterPresent bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "after-ordinal" {
			afterPresent = true
		}
	})
	method, path := http.MethodPost, "/v1/lake/snapshots"
	var input, response proto.Message
	switch *operation {
	case "create":
		if len(*requestID) < 16 || len(*requestID) > 128 || *snapshotID != "" || afterPresent {
			return errors.New("create requires a stable request ID and selection only")
		}
		start, err1 := time.Parse(time.RFC3339Nano, *from)
		end, err2 := time.Parse(time.RFC3339Nano, *until)
		if err1 != nil || err2 != nil || !end.After(start) {
			return errors.New("invalid exact event-time interval")
		}
		kinds := strings.Split(*types, ",")
		seen := map[string]bool{}
		if len(kinds) > 64 {
			return errors.New("event-type count exceeds bound")
		}
		for _, kind := range kinds {
			if kind == "" || len(kind) > 256 || strings.TrimSpace(kind) != kind || seen[kind] {
				return errors.New("invalid exact event types")
			}
			seen[kind] = true
		}
		input = &lakepb.CreateSnapshotInput{RequestId: *requestID, Selection: &lakepb.SnapshotSelection{EventTypes: kinds, EventTimeFrom: timestamppb.New(start), EventTimeUntil: timestamppb.New(end)}}
		response = new(lakepb.CreateSnapshotResponse)
	case "get", "list", "count", "failures":
		if _, err := uuid.Parse(*snapshotID); err != nil || *requestID != "" || *from != "" || *until != "" || *types != "" {
			return errors.New("read requires a snapshot UUID, not a new selection")
		}
		path += "/" + *snapshotID
		if *operation == "get" {
			if afterPresent {
				return errors.New("metadata has no ordinal cursor")
			}
			method, response = http.MethodGet, new(lakepb.GetSnapshotResponse)
			break
		}
		if *page == 0 || *page > 1000 || (*operation == "count" && afterPresent) {
			return errors.New("invalid finite query page")
		}
		q := &lakepb.SnapshotQuery{Operation: lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, PageSize: uint32(*page)}
		if *operation == "count" {
			q.Operation = lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE
		}
		if *operation == "failures" {
			q.Operation = lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_DECODE_FAILURES
		}
		if afterPresent {
			q.AfterOrdinal = after
		}
		path += "/query"
		input, response = q, new(lakepb.QuerySnapshotResponse)
	default:
		return errors.New("choose create, get, list, count, or failures")
	}
	var body []byte
	if input != nil {
		body, err = protojson.Marshal(input)
		if err != nil || len(body) > 64<<10 {
			return errors.New("snapshot input exceeds contract")
		}
	}
	// Credentials remain runtime-only; no token/signing-key command-line flags.
	token, err := secret.Read("KANZ_GATEWAY_TOKEN")
	if err != nil || token == "" {
		return errors.New("authenticated gateway token unavailable")
	}
	signing, err := secret.Read("KANZ_GATEWAY_SIGNING_SECRET")
	if err != nil {
		return errors.New("gateway signing configuration unavailable")
	}
	client, err := gatewayclient.New(gatewayclient.Config{BaseURL: *endpoint, Token: gatewayclient.StaticToken(token), SigningSecret: signing, MaxResponseBytes: 1 << 20})
	if err != nil {
		return errors.New("gateway client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := client.Do(ctx, method, path, nil, body)
	if err != nil {
		var refused *gatewayclient.StatusError
		if errors.As(err, &refused) {
			return fmt.Errorf("snapshot request refused (HTTP %d)", refused.Status)
		}
		return errors.New("snapshot response unconfirmed; reuse the same create request ID and input")
	}
	if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, response) != nil {
		return errors.New("snapshot response invalid")
	}
	canonical, err := (protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}).Marshal(response)
	if err != nil || len(canonical) > 1<<20 {
		return errors.New("snapshot response exceeds contract")
	}
	_, err = output.Write(append(canonical, '\n'))
	return err
}
