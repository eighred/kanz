package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

func capitalCashHistory(cfg config.Config, source transport.Source) (capital.HistoryReader, error) {
	if cfg.CashHistoryGateway == "" {
		return nil, nil
	}
	if source == nil {
		return nil, errors.New("OMS_CASH_HISTORY_GATEWAY requires the active OMS SPIFFE identity")
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
	gatewayID, err := spiffeid.FromString("spiffe://kanz.internal/ns/kanz-services/sa/api-gateway")
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// A proxy would see the bearer or could redirect the service connection;
	// this is an in-cluster SVID-authorized hop, not general outbound HTTP.
	tr.Proxy = nil
	tr.TLSClientConfig = transport.ClientTLSConfig(source, transport.AuthorizeServices(gatewayID))
	tr.MaxConnsPerHost = 8
	tr.MaxIdleConnsPerHost = 8
	client := &http.Client{Transport: tr}
	return capital.NewHistoryClient(cfg.CashHistoryGateway, client, func(context.Context) (string, error) {
		return secret.Read("OMS_CASH_HISTORY_TOKEN")
	})
}
