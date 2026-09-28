package delivery

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run owns one bounded sender per replica. LISTEN wakes durable work after
// commit; startup and lease-deadline scans recover missed notifications/crashes.
// No credential or token is persisted in the queue or notification payload.
func Run(ctx context.Context, pool *pgxpool.Pool, store *identity.Postgres, sender *SMTP, origin string, logger *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return errors.New("mail notification connection unavailable")
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `LISTEN identity_mail`); err != nil {
		return errors.New("mail notification subscription failed")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, e := conn.Exec(cleanup, `UNLISTEN identity_mail`); e != nil {
			_ = conn.Conn().Close(cleanup)
		}
	}()
	for ctx.Err() == nil {
		m, e := store.ClaimMail(ctx, time.Now().UTC())
		if e != nil {
			return errors.New("mail claim failed")
		}
		if m != nil {
			path := "/verify-mailbox"
			subject := "Verify your Kanz recovery mailbox"
			if m.Purpose == "recover" {
				path = "/recover"
				subject = "Reset your Kanz password"
			}
			body := "Open this link to continue:\n" + origin + path + "#token=" + url.QueryEscape(m.Token) + "\n\nThis link expires at " + m.ExpiresAt.UTC().Format(time.RFC3339) + " and can be used once. If you did not request it, ignore this message."
			e = sender.Send(ctx, m.Address, subject, body)
			if e != nil {
				logger.Warn("identity mail attempt refused", "request_id", m.ID, "attempt", m.Attempt)
			}
			if err = store.FinishMail(ctx, m, e == nil, time.Now().UTC()); err != nil {
				return errors.New("mail outcome persistence failed")
			}
			continue
		}
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = conn.Conn().WaitForNotification(wait)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return errors.New("mail notification connection lost")
		}
	}
	return nil
}
