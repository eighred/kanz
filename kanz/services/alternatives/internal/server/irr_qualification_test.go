package server

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func irrPositionResponse(t *testing.T, st fund.Store, id string) map[string]any {
	t.Helper()
	r := httptest.NewRecorder()
	New(&Readiness{}, nil, st).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/commitments/"+id, nil))
	if r.Code != http.StatusOK {
		t.Fatalf("position status = %d", r.Code)
	}
	var result map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func irrServedFixtures(t *testing.T, st fund.Store) {
	t.Helper()
	start := day(2002, 1, 1)
	for _, tc := range []struct {
		name, call, nav string
		end             time.Time
		want            float64
		absent          bool
		distribute      bool
		earlyCall       string
	}{
		{"tiny", "0.000000000001", "0.0000000000011", start.AddDate(1, 0, 0), .1, false, false, ""},
		{"ordinary", "100", "110", start.AddDate(1, 0, 0), .1, false, false, ""},
		{"zero", "100", "100", start.AddDate(1, 0, 0), 0, false, false, ""},
		{"same_instant", "100", "100", start, 0, true, false, ""},
		{"small_residual", "1", "0.00000000000000000001", start.Add(2 * time.Microsecond), .00000031536004972597000989, false, true, ""},
		{"translated_residual", "1", "1e-20", start.Add(2 * time.Microsecond), .00000031536004972597000989, false, true, "1e-127"},
		{"negative_cluster", "1e-100", "1e-120", start.Add(2 * time.Microsecond), -.12330568412790002188, false, true, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, ok := new(big.Rat).SetString(tc.call)
			if !ok {
				t.Fatal("invalid call fixture")
			}
			nav, ok := new(big.Rat).SetString(tc.nav)
			if !ok {
				t.Fatal("invalid NAV fixture")
			}
			events := []*alternatives.Event{
				{EventID: tc.name + "-call", CommitmentID: tc.name, Type: alternatives.EventCall, Amount: call, Date: start},
				{EventID: tc.name + "-nav", CommitmentID: tc.name, Type: alternatives.EventNAVMark, Amount: nav, Date: tc.end},
			}
			if tc.distribute {
				events = append(events, &alternatives.Event{EventID: tc.name + "-distribution", CommitmentID: tc.name, Type: alternatives.EventDistribution, Amount: new(big.Rat).Set(call), Date: start.Add(time.Microsecond)})
			}
			if tc.earlyCall != "" {
				early, ok := new(big.Rat).SetString(tc.earlyCall)
				if !ok {
					t.Fatal("invalid early call fixture")
				}
				events = append(events, &alternatives.Event{EventID: tc.name + "-early", CommitmentID: tc.name, Type: alternatives.EventCall, Amount: early, Date: day(2, 1, 1)})
			}
			for _, e := range events {
				if err := st.Append(context.Background(), e); err != nil {
					t.Fatal(err)
				}
				if err := st.Append(context.Background(), e); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := st.Journal(context.Background(), tc.name)
			if err != nil || len(rows) != len(events) {
				t.Fatalf("journal rows = %d, %v; want %d idempotent records", len(rows), err, len(events))
			}
			result := irrPositionResponse(t, st, tc.name)
			value, present := result["irr"]
			if tc.absent {
				if present {
					t.Fatalf("unidentifiable IRR was served: %v", value)
				}
			} else if got, ok := value.(float64); !ok || math.Abs(got-tc.want) > 1e-10 {
				t.Fatalf("served IRR = %v; want %g within 1e-10", value, tc.want)
			}
			if result["tvpi"] != 1.1 && tc.name == "ordinary" {
				t.Fatalf("independent capital multiple changed: %v", result["tvpi"])
			}
		})
	}
}

func TestIRRServedQualification(t *testing.T) {
	irrServedFixtures(t, fund.NewMemoryStore())
}

func TestIRRPostgresServedRestartAndIsolation(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL required for durable IRR served proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal("create disposable database pool")
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatal("durable IRR proof requires NOSUPERUSER NOBYPASSRLS")
	}
	schema := "irr_" + uuid.NewString()[:8]
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("remove disposable IRR schema:", err)
		}
	})
	open := func(tenant string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal("parse disposable database configuration")
		}
		cfg.MaxConns = 2
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal("open scoped IRR pool")
		}
		t.Cleanup(p.Close)
		return p
	}
	pool := open("irr-owner")
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) != 2 {
		t.Fatalf("expected both alternatives owner migrations, got %d: %v", len(files), err)
	}
	sort.Strings(files)
	for _, file := range files {
		ddl, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	st := fund.NewPostgres(pool)
	irrServedFixtures(t, st)
	pool.Close()
	restarted := fund.NewPostgres(open("irr-owner"))
	for _, id := range []string{"tiny", "ordinary", "zero", "same_instant", "small_residual", "translated_residual", "negative_cluster"} {
		t.Run("restart_"+id, func(t *testing.T) {
			result := irrPositionResponse(t, restarted, id)
			if id == "same_instant" {
				if _, present := result["irr"]; present {
					t.Fatal("restart invented an annual rate")
				}
			} else {
				want := .1
				if id == "zero" {
					want = 0
				} else if id == "small_residual" || id == "translated_residual" {
					want = .00000031536004972597000989
				} else if id == "negative_cluster" {
					want = -.12330568412790002188
				}
				got, ok := result["irr"].(float64)
				if !ok || math.Abs(got-want) > 1e-10 {
					t.Fatalf("restart rate = %v, want %g", result["irr"], want)
				}
			}
		})
	}
	t.Run("other_tenant", func(t *testing.T) {
		other := fund.NewPostgres(open("irr-other"))
		rows, err := other.Journal(ctx, "ordinary")
		if err != nil || len(rows) != 0 {
			t.Fatalf("other tenant observed %d rows: %v", len(rows), err)
		}
		if _, present := irrPositionResponse(t, other, "ordinary")["irr"]; present {
			t.Fatal("other tenant discovered the owner's rate")
		}
	})
}
