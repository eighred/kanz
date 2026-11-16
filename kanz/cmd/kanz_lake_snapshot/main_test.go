package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestOperatorSnapshotCommandsUseAuthenticatedGatewayOnly(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_TOKEN", "synthetic-test-token")
	t.Setenv("KANZ_GATEWAY_TOKEN_FILE", "")
	t.Setenv("KANZ_GATEWAY_SIGNING_SECRET", "")
	t.Setenv("KANZ_GATEWAY_SIGNING_SECRET_FILE", "")
	const id = "00000000-0000-4000-8000-000000000001"
	for _, operation := range []string{"create", "get", "list", "count", "failures"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer synthetic-test-token" || r.Header.Get("X-Kanz-Principal-Tenant") != "" {
					t.Error("operator supplied authority rather than bearer credential")
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
				if err != nil {
					t.Error("read failed")
				}
				w.Header().Set("Content-Type", "application/json")
				switch operation {
				case "create":
					var input lakepb.CreateSnapshotInput
					if r.Method != "POST" || r.URL.Path != "/v1/lake/snapshots" || protojson.Unmarshal(body, &input) != nil || input.Selection.EventTimeFrom.Nanos != 123456789 || input.Selection.EventTimeUntil.Nanos != 123456790 {
						t.Error("business selection/precision changed")
					}
					_, _ = w.Write([]byte(`{"snapshot":{"snapshotId":"` + id + `","state":"SNAPSHOT_STATE_REQUESTED"}}`))
				case "get":
					if r.Method != "GET" || r.URL.Path != "/v1/lake/snapshots/"+id || len(body) != 0 {
						t.Error("metadata route changed")
					}
					_, _ = w.Write([]byte(`{"snapshot":{"snapshotId":"` + id + `","state":"SNAPSHOT_STATE_READY"}}`))
				default:
					var query lakepb.SnapshotQuery
					if r.Method != "POST" || r.URL.Path != "/v1/lake/snapshots/"+id+"/query" || protojson.Unmarshal(body, &query) != nil || query.PageSize != 2 {
						t.Error("query contract changed")
					}
					if operation == "list" && (query.AfterOrdinal == nil || *query.AfterOrdinal != 0) {
						t.Error("explicit zero cursor lost")
					}
					if operation == "count" && query.AfterOrdinal != nil {
						t.Error("count invented ordinal cursor")
					}
					_, _ = w.Write([]byte(`{"result":{"exhausted":true}}`))
				}
			}))
			defer server.Close()
			args := []string{"-gateway", server.URL, "-operation", operation}
			if operation == "create" {
				args = append(args, "-request-id", "stable-request-fixture", "-from", "2026-11-07T00:00:00.123456789Z", "-until", "2026-11-07T00:00:00.123456790Z", "-event-types", "market.trade")
			} else {
				args = append(args, "-snapshot-id", id, "-page-size", "2")
			}
			if operation == "list" {
				args = append(args, "-after-ordinal", "0")
			}
			var output bytes.Buffer
			if err := run(args, &output); err != nil || calls != 1 || output.Len() == 0 {
				t.Fatal("operator command failed")
			}
		})
	}
}

func TestOperatorSnapshotRefusesPathsAndDoesNotPrintUpstreamError(t *testing.T) {
	for _, args := range [][]string{
		{"-gateway", "http://example.invalid", "-operation", "get"},
		{"-gateway", "https://example.invalid", "-operation", "get", "-snapshot-id", "../../raw"},
		{"-gateway", "https://example.invalid", "-operation", "count", "-snapshot-id", "00000000-0000-4000-8000-000000000001", "-after-ordinal", "0"},
	} {
		if run(args, io.Discard) == nil {
			t.Fatal("invalid command accepted")
		}
	}
	t.Setenv("KANZ_GATEWAY_TOKEN", "synthetic-test-token")
	t.Setenv("KANZ_GATEWAY_TOKEN_FILE", "")
	t.Setenv("KANZ_GATEWAY_SIGNING_SECRET", "")
	t.Setenv("KANZ_GATEWAY_SIGNING_SECRET_FILE", "")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"untrusted-sensitive-detail"}`))
	}))
	defer s.Close()
	err := run([]string{"-gateway", s.URL, "-operation", "get", "-snapshot-id", "00000000-0000-4000-8000-000000000001"}, io.Discard)
	if err == nil || strings.Contains(err.Error(), "untrusted-sensitive-detail") {
		t.Fatal("upstream text escaped closed error")
	}
}
