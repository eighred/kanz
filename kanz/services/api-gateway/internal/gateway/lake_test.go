package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"google.golang.org/grpc"
)

type lakeClientFixture struct {
	calls  int
	caller *lakepb.GatewayCaller
}

func (f *lakeClientFixture) CreateSnapshot(_ context.Context, r *lakepb.CreateSnapshotRequest, _ ...grpc.CallOption) (*lakepb.CreateSnapshotResponse, error) {
	f.calls++
	f.caller = r.Caller
	return &lakepb.CreateSnapshotResponse{}, nil
}
func (f *lakeClientFixture) GetSnapshot(_ context.Context, r *lakepb.GetSnapshotRequest, _ ...grpc.CallOption) (*lakepb.GetSnapshotResponse, error) {
	f.calls++
	f.caller = r.Caller
	return &lakepb.GetSnapshotResponse{}, nil
}
func (f *lakeClientFixture) QuerySnapshot(_ context.Context, r *lakepb.QuerySnapshotRequest, _ ...grpc.CallOption) (*lakepb.QuerySnapshotResponse, error) {
	f.calls++
	f.caller = r.Caller
	return &lakepb.QuerySnapshotResponse{}, nil
}

func TestLakeGatewayBusinessDTOAndAuthenticatedCaller(t *testing.T) {
	for _, route := range []struct {
		method, path, body string
		cap                authz.Capability
	}{
		{"POST", "/v1/lake/snapshots", `{"requestId":"request-fixture"}`, authz.LakeSnapshotCreate},
		{"GET", "/v1/lake/snapshots/fixture", "", authz.LakeSnapshotRead},
		{"POST", "/v1/lake/snapshots/fixture/query", `{"operation":"SNAPSHOT_QUERY_OPERATION_LIST_EVENTS","pageSize":1}`, authz.LakeSnapshotRead},
	} {
		t.Run(route.path, func(t *testing.T) {
			for _, variant := range []string{"valid", "foreign", "portfolio", "generic-read", "missing", "caller-injection"} {
				t.Run(variant, func(t *testing.T) {
					if variant == "caller-injection" && route.method == "GET" {
						return
					}
					client := new(lakeClientFixture)
					h := New(nil, nil, nil, "", nil)
					if h.WithLakeSnapshots(client, "tenant-a") != nil {
						t.Fatal("bind failed")
					}
					mux := authz.NewMux(authz.Grants{"explicit": {route.cap}, "generic": {authz.Read}}, nil)
					h.Routes(mux)
					p := &middleware.Principal{Subject: "human", Tenant: "tenant-a", Roles: []string{"explicit"}}
					body := route.body
					switch variant {
					case "foreign":
						p.Tenant = "tenant-b"
					case "portfolio":
						p.Portfolios = []string{"restricted"}
					case "generic-read":
						p.Roles = []string{"generic"}
					case "missing":
						p = nil
					case "caller-injection":
						body = `{"caller":{"subject":"forged","tenant":"tenant-b"}}`
					}
					req := httptest.NewRequest(route.method, route.path, strings.NewReader(body))
					req.Header.Set("X-Kanz-Principal-Tenant", "tenant-b")
					req.Header.Set("X-Kanz-Principal-Subject", "forged")
					if p != nil {
						req = req.WithContext(middleware.WithPrincipal(req.Context(), p))
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if variant == "valid" {
						if w.Code != http.StatusOK || client.calls != 1 || client.caller.Subject != "human" || client.caller.Tenant != "tenant-a" || len(client.caller.Portfolios) != 0 || client.caller.Roles[0] != "explicit" {
							t.Fatal("authenticated projection changed")
						}
					} else if client.calls != 0 || w.Code < 400 {
						t.Fatal("unauthorized input reached upstream")
					}
				})
			}
		})
	}
}
