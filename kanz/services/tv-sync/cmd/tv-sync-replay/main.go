// tv-sync-replay qualifies one restored SQL snapshot without publishing or writing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

func main() { os.Exit(run(os.Stdout)) }

func run(out io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fatal := lifecycle.NewFatal(stop)
	if runVerifier(ctx, out) != 0 {
		fatal.Raise(errors.New("replay verification refused"))
	}
	return fatal.Code()
}

func runVerifier(parent context.Context, out io.Writer) int {
	// No command-line credentials or alternative target flags. The operator binds
	// this stamped image, tenant and mounted DSN to the reviewed restored target.
	if len(os.Args) != 1 {
		return refuse(out, "invalid_configuration")
	}
	expected := os.Getenv("TV_SYNC_REPLAY_EXPECTED_VERSION")
	if !version.IsStamped() || expected == "" || expected != version.String() || strings.Contains(expected, "dirty") {
		return refuse(out, "source_mismatch")
	}
	tenant := os.Getenv("TV_SYNC_TENANT")
	retention, err := time.ParseDuration(os.Getenv("TV_SYNC_RETENTION"))
	if tenant == "" || len(tenant) > 128 || err != nil || retention < 24*time.Hour || retention > 365*24*time.Hour {
		return refuse(out, "invalid_configuration")
	}
	if os.Getenv("TV_SYNC_DATABASE_URL_FILE") == "" || os.Getenv("TV_SYNC_DATABASE_URL") != "" {
		return refuse(out, "mounted_database_required")
	}
	dsn, err := secret.ReadBounded("TV_SYNC_DATABASE_URL", 16<<10)
	if err != nil || dsn == "" {
		return refuse(out, "database_input_refused")
	}
	ctx, cancel := context.WithTimeout(parent, projection.ReplayDeadline)
	defer cancel()
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		return refuse(out, "database_unavailable")
	}
	defer pool.Close()
	proof, err := projection.VerifyPostgresSnapshot(ctx, pool, tenant, retention)
	if err != nil {
		code := "snapshot_refused"
		if err == projection.ErrReplayNoHistory {
			code = "no_history_not_restored"
		}
		if err == projection.ErrReplayNoCheckpoint {
			code = "no_checkpoint"
		}
		if err == projection.ErrReplayMismatch {
			code = "replay_mismatch"
		}
		if err == projection.ErrReplayLimit {
			code = "bounded_domain_exceeded"
		}
		return refuse(out, code)
	}
	proof.SourceVersion = version.String()
	if json.NewEncoder(out).Encode(proof) != nil {
		return 2
	}
	return 0
}

func refuse(out io.Writer, code string) int {
	// Only closed codes reach stdout; errors may contain DSNs, paths or records.
	_ = json.NewEncoder(out).Encode(struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}{"REFUSED", code})
	return 2
}
