// Package delivery owns the identity service's outbound mail boundary.
package delivery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// ErrDelivery deliberately excludes server replies: they may echo recipients,
// authentication material or message content. Callers must not log wire errors.
var ErrDelivery = errors.New("identity: SMTP delivery failed")

type Config struct {
	Address  string
	From     string
	Username string
	Password string
	// Mode is either starttls or tls. Cleartext delivery is never supported.
	Mode string
}

func Mailbox(value string) bool {
	if len(value) > 254 || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	a, err := mail.ParseAddress(value)
	return err == nil && a.Name == "" && a.Address == value && strings.Contains(value, "@")
}

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Address)
	if err != nil || host == "" || port == "" {
		return errors.New("SMTP address must contain host and port")
	}
	number, portErr := strconv.Atoi(port)
	if portErr != nil || number < 1 || number > 65535 {
		return errors.New("SMTP port must be between 1 and 65535")
	}
	if !Mailbox(c.From) {
		return errors.New("SMTP sender must be a bare mailbox address")
	}
	if c.Mode != "starttls" && c.Mode != "tls" {
		return errors.New("SMTP TLS mode must be starttls or tls")
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("SMTP username and password must be configured together")
	}
	return nil
}

type SMTP struct {
	config    Config
	tlsConfig *tls.Config
}

func New(c Config) (*SMTP, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(c.Address)
	return &SMTP{config: c, tlsConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}, nil
}

// Send succeeds only after SMTP accepts DATA. It does not attest inbox delivery
// or mailbox ownership. The caller must require a separate one-time challenge.
// A single deadline bounds DNS, connect, TLS, AUTH and DATA; cancellation closes
// the socket even when the peer stops responding midway through a command.
func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	if !Mailbox(to) || strings.ContainsAny(subject, "\r\n\x00") || len(subject) > 120 || len(body) > 8192 {
		return ErrDelivery
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.config.Address)
	if err != nil {
		return ErrDelivery
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return ErrDelivery
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); close(stopped) })
	defer func() {
		if !stop() {
			<-stopped
		}
	}()
	var wire net.Conn = conn
	if s.config.Mode == "tls" {
		secure := tls.Client(conn, s.tlsConfig.Clone())
		if err = secure.HandshakeContext(ctx); err != nil {
			return ErrDelivery
		}
		wire = secure
	}
	client, err := smtp.NewClient(wire, s.tlsConfig.ServerName)
	if err != nil {
		return ErrDelivery
	}
	defer func() { _ = client.Close() }()
	if s.config.Mode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return ErrDelivery
		}
		if err = client.StartTLS(s.tlsConfig.Clone()); err != nil {
			return ErrDelivery
		}
	}
	if s.config.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", s.config.Username, s.config.Password, s.tlsConfig.ServerName)); err != nil {
			return ErrDelivery
		}
	}
	if err = client.Mail(s.config.From); err != nil {
		return ErrDelivery
	}
	if err = client.Rcpt(to); err != nil {
		return ErrDelivery
	}
	data, err := client.Data()
	if err != nil {
		return ErrDelivery
	}
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", s.config.From, to, subject, strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	if _, err = data.Write([]byte(message)); err != nil {
		return ErrDelivery
	}
	if err = data.Close(); err != nil {
		return ErrDelivery
	}
	// DATA's acknowledgement is the commit point. QUIT failure must not cause
	// another delivery after the server has already accepted the message.
	return nil
}
