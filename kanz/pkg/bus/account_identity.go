package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

var ErrAccountIdentity = errors.New("nats: authenticated account identity unavailable or mismatched")

// RequireAccountIdentity checks the account actually assigned to this existing
// connection by the authenticated broker. Identical stream profiles in different
// accounts cannot substitute for this check. Static account names and the exact
// configured workload user are required; no display-name or envelope fallback.
// Servers without the system user-info service refuse, rather than infer identity.
func (c *NATSClient) RequireAccountIdentity(ctx context.Context, account, user string) error {
	if c == nil || c.conn == nil || account == "" || len(account) > 256 || user == "" || len(user) > 256 {
		return ErrAccountIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reply, err := c.conn.RequestWithContext(ctx, "$SYS.REQ.USER.INFO", nil)
	if err != nil || reply == nil {
		return ErrAccountIdentity
	}
	return checkAccountIdentity(reply.Data, account, user)
}

func checkAccountIdentity(raw []byte, account, user string) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return ErrAccountIdentity
	}
	var response struct {
		Error json.RawMessage `json:"error"`
		Data  *struct {
			Account string `json:"account"`
			User    string `json:"user"`
		} `json:"data"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if d.Decode(&response) != nil || d.Decode(new(any)) != io.EOF || (len(response.Error) != 0 && !bytes.Equal(response.Error, []byte("null"))) || response.Data == nil || response.Data.Account != account || response.Data.User != user {
		return ErrAccountIdentity
	}
	return nil
}
