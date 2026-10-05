package main

import (
	"context"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/services/risk-engine/internal/replay"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newRiskEvaluator(ctx context.Context, pool *pgxpool.Pool, registry *compute.Registry) (engine.Evaluator, error) {
	if pool == nil {
		return nil, nil
	}
	repository := replay.NewPostgres(pool)
	if err := repository.Check(ctx); err != nil {
		return nil, err
	}
	return replay.New(registry, repository, version.String())
}
