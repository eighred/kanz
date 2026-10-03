package main

import (
	"context"
	"errors"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/config"
)

func capitalCashHistory(cfg config.Config) (capital.HistoryReader, error) {
	if cfg.CashHistoryGateway == "" {
		return nil, nil
	}
	// Validate the declared secret mount before subscribing. Read it again per
	// request so a rotated short-lived gateway credential takes effect promptly.
	value, err := secret.Read("OMS_CASH_HISTORY_TOKEN")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("OMS_CASH_HISTORY_GATEWAY requires OMS_CASH_HISTORY_TOKEN or its secret mount")
	}
	return capital.NewHistoryClient(cfg.CashHistoryGateway, nil, func(context.Context) (string, error) {
		return secret.Read("OMS_CASH_HISTORY_TOKEN")
	})
}
