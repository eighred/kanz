package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/filing"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/regulatory/internal/recording"
	"github.com/google/uuid"
)

// filingModel is HTTP/assembler model evidence only. It does not substitute for
// the dedicated real PostgreSQL transaction/restart and NATS readback controls.
type filingModel struct {
	mu        sync.Mutex
	responses map[string][]byte
	calls     int
}

func (m *filingModel) Record(_ context.Context, plan recording.Plan) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	key := plan.Tenant + "/" + plan.Portfolio + "/" + plan.Framework + "/" + plan.AsOf.UTC().Format(time.RFC3339Nano) + "/" + plan.InputDigest
	if m.responses != nil && m.responses[key] != nil {
		return append([]byte(nil), m.responses[key]...), nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	body, out, err := plan.Build(filing.PublicationScope{Tenant: plan.Tenant, Portfolio: plan.Portfolio, ReportID: id.String(), InputDigest: plan.InputDigest, AsOf: plan.AsOf, RecordedAt: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}, filing.HashSigner{})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", recording.ErrInvalidInput, err)
	}
	body.SignatureKind = regulatorypb.FilingSignatureKind_FILING_SIGNATURE_KIND_CONTENT_HASH
	if err := regulatorypublish.ValidateBody(body); err != nil {
		return nil, err
	}
	out["report_id"] = id.String()
	response, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if m.responses == nil {
		m.responses = make(map[string][]byte)
	}
	m.responses[key] = response
	return append([]byte(nil), response...), nil
}

func filingModelOptions() []Option {
	return []Option{WithRecorder("__system__", "http-model", &filingModel{})}
}

func filingPrincipal(r *http.Request, tenant string) {
	auth.SetPrincipalHeaders(r.Header, "fixture:filing", tenant, []string{"compliance"})
}

func omitNullModelFields(t *testing.T, raw []byte) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	var omit func(any)
	omit = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if child == nil {
					delete(v, key)
				} else {
					omit(child)
				}
			}
		case []any:
			for _, child := range v {
				omit(child)
			}
		}
	}
	omit(value)
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
