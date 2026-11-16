package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/identity"
)

func TestAdministrativeRevisionJSONDomain(t *testing.T) {
	for _, value := range []int64{0, 1<<53 - 1, 1 << 53, 1<<53 + 1, math.MaxInt64} {
		text := strconv.FormatInt(value, 10)
		var decoded revisionJSON
		if err := json.Unmarshal([]byte(strconv.Quote(text)), &decoded); err != nil || int64(decoded) != value {
			t.Fatalf("revision decimal did not round trip: %s", text)
		}
	}
	for _, value := range []string{`0`, `7`, `9007199254740993`, `null`, `""`, `"-1"`, `"+1"`, `"01"`, `" 1"`, `"1.0"`, `"1e3"`, `"9223372036854775808"`, `[]`, `{}`} {
		var decoded revisionJSON
		if err := json.Unmarshal([]byte(value), &decoded); err == nil {
			t.Fatalf("unsafe revision representation accepted: %s", value)
		}
	}
}

func TestAdministrativeHTTPRefusesMalformedRevisionsBeforeMutation(t *testing.T) {
	for _, route := range []struct{ method, path, fields string }{
		{"PUT", "/users/alice/access", `,"roles":["kanz-user"],"portfolios":[]`},
		{"POST", "/invites/source/revoke", ""}, {"POST", "/invites/source/reissue", ""},
	} {
		f := newProvServer(t, operatorClaims(), nil)
		bodies := []string{`{}`, `null`, `{"revision":"1"` + route.fields + `} {}`}
		for _, raw := range []string{`0`, `null`, `"-1"`, `"+1"`, `"01"`, `"1.0"`, `"1e3"`, `"9223372036854775808"`} {
			bodies = append(bodies, `{"revision":`+raw+route.fields+`}`)
		}
		mux := http.NewServeMux()
		f.s.Routes(mux)
		for _, body := range bodies {
			r := httptest.NewRequest(route.method, route.path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer tok")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s accepted malformed revision: %d", route.path, w.Code)
			}
		}
		if f.store.inviteChanges != 0 || f.store.accessChanges != 0 {
			t.Fatal("malformed revision reached administrative mutation")
		}
	}
}

func TestAdministrativeHTTPProjectionLeavesDomainAuditJSONStable(t *testing.T) {
	a := &identity.Access{Revision: 1<<53 + 1, SessionEpoch: math.MaxInt64}
	i := identity.InvitationSummary{Revision: math.MaxInt64}
	for _, pair := range []struct {
		wire, domain any
		revision     string
	}{
		{accessResponse(a), a, "9007199254740993"}, {invitationResponse(i), i, "9223372036854775807"},
	} {
		wire, err := json.Marshal(pair.wire)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(wire, &fields) != nil || string(fields["revision"]) != strconv.Quote(pair.revision) {
			t.Fatal("HTTP revision is not exact decimal text")
		}
		domain, err := json.Marshal(pair.domain)
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(domain, &fields) != nil || string(fields["revision"]) != pair.revision {
			t.Fatal("domain audit JSON contract changed")
		}
	}
	wire, err := json.Marshal(accessResponse(a))
	if err != nil || !strings.Contains(string(wire), `"session_epoch":"9223372036854775807"`) {
		t.Fatal("HTTP session epoch was not projected losslessly")
	}
}
