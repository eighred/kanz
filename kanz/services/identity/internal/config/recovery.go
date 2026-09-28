package config

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/identity/internal/delivery"
)

type Recovery struct {
	SMTP   delivery.Config
	Origin string
}

func loadRecovery() (*Recovery, error) {
	enabled := os.Getenv("IDENTITY_RECOVERY_ENABLED")
	if enabled == "" || enabled == "false" {
		return nil, nil
	}
	if enabled != "true" {
		return nil, errors.New("IDENTITY_RECOVERY_ENABLED must be true or false")
	}
	password, err := secret.Read("IDENTITY_SMTP_PASSWORD")
	if err != nil {
		return nil, err
	}
	c := &Recovery{SMTP: delivery.Config{Address: os.Getenv("IDENTITY_SMTP_ADDRESS"), From: os.Getenv("IDENTITY_SMTP_FROM"), Mode: os.Getenv("IDENTITY_SMTP_TLS"), Username: os.Getenv("IDENTITY_SMTP_USERNAME"), Password: password}, Origin: os.Getenv("IDENTITY_RECOVERY_ORIGIN")}
	if err = c.SMTP.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("IDENTITY_RECOVERY_ORIGIN must be a bare HTTPS origin")
	}
	c.Origin = strings.TrimRight(c.Origin, "/")
	return c, nil
}
