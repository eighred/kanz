package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPostgresTenantEvidencePagingAndWindow(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	ctx := context.Background()
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	var super, bypass bool
	if err := boot.QueryRow(ctx, `SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatal("real restricted test role required", err)
	}
	schema := fmt.Sprintf("audit_export_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := boot.Exec(ctx, `DROP SCHEMA `+quoted+` CASCADE`); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal(err)
	}
	for _, file := range files {
		ddl, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	st := audit.NewPostgres(pool)
	for i, tenant := range []string{"acme", "other", "acme", "other", "acme"} {
		_, err := st.Append(ctx, &audit.Record{EventID: fmt.Sprintf("test-%d", i), TenantID: tenant, Kind: audit.KindAuthzDecision, OccurredAt: time.Unix(1700000000+int64(i), 0), RecordedAt: time.Unix(1700000000+int64(i), 0), Summary: "test fixture"})
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := newAuditServer(st)
	srv.verifyRoles = []string{"estate-verifier"}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		req := httptest.NewRequest("GET", "/v2/audit/reports/full-log?limit=1&after="+cursor, nil)
		auth.SetPrincipalHeaders(req.Header, "test:reader", "acme", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
		var body struct {
			Complete bool   `json:"complete"`
			Next     string `json:"next_cursor"`
			Records  []struct {
				ID     string `json:"event_id"`
				Tenant string `json:"tenant_id"`
				Seq    string `json:"seq"`
			} `json:"records"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Records) != 1 || body.Records[0].Tenant != "acme" || seen[body.Records[0].ID] || body.Records[0].Seq == "" {
			t.Fatal("invalid tenant page", body)
		}
		seen[body.Records[0].ID] = true
		if body.Complete != (page == 2) {
			t.Fatal("incorrect completeness", body)
		}
		cursor = body.Next
	}
	req := httptest.NewRequest("GET", "/v2/soc2/evidence?from=2023-11-14T22:13:20Z&to=2023-11-14T22:13:22Z", nil)
	auth.SetPrincipalHeaders(req.Header, "test:reader", "acme", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	var evidence struct {
		Total      int    `json:"total_count"`
		Assessment string `json:"assessment"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &evidence); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 409 || evidence.Total != 2 || evidence.Assessment != "not_assessed" {
		t.Fatal(rr.Code, rr.Body.String())
	}
}
