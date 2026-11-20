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
	if _, err := historyCredentials(context.Background()); err != nil {
		return nil, err
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
	return capital.NewHistoryClient(cfg.CashHistoryGateway, client, historyCredentials)
}

func historyCredentials(_ context.Context) (capital.HistoryCredentials, error) {
	token, err := secret.Read("OMS_CASH_HISTORY_TOKEN")
	if err != nil {
		return capital.HistoryCredentials{}, err
	}
	key, err := secret.Read("OMS_CASH_HISTORY_SIGNING_SECRET")
	if err != nil {
		return capital.HistoryCredentials{}, err
	}
	if strings.TrimSpace(token) == "" || strings.TrimSpace(key) == "" {
		return capital.HistoryCredentials{}, errors.New("OMS cash history requires a bearer and its dedicated request signing key")
	}
	return capital.HistoryCredentials{Bearer: token, SigningKey: key}, nil
}
