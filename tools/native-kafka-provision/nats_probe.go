// The tenant's own account must serve its acknowledged data under a stream
// name shared by both fixture tenants.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
)

func verify(url, creds, tenant, mode string) error {
	nc, err := nats.Connect(url, nats.UserCredentials(creds), nats.Timeout(3*time.Second), nats.NoReconnect())
	if err != nil {
		return fmt.Errorf("authenticate probe: %w", err)
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(3 * time.Second))
	if err != nil {
		return err
	}
	info, err := js.AccountInfo()
	if err != nil {
		return err
	}
	if info.Limits.MaxMemory != 64_000_000 || info.Limits.MaxStore != 2_000_000_000 {
		return fmt.Errorf("unexpected JetStream account limits")
	}
	const stream = "LIFECYCLE"
	const subject = "fixture.lifecycle"
	if mode == "create" {
		if _, err := js.AddStream(&nats.StreamConfig{
			Name: stream, Subjects: []string{subject}, Storage: nats.MemoryStorage,
		}); err != nil {
			return err
		}
		ack, err := js.Publish(subject, []byte(tenant))
		if err != nil {
			return err
		}
		if ack.Stream != stream || ack.Sequence != 1 {
			return fmt.Errorf("first publish did not start a tenant-local stream")
		}
	}
	state, err := js.StreamInfo(stream)
	if err != nil {
		return err
	}
	if state.State.Msgs != 1 || state.State.FirstSeq != 1 || state.State.LastSeq != 1 {
		return fmt.Errorf("expected one tenant-local record")
	}
	record, err := js.GetMsg(stream, 1)
	if err != nil {
		return err
	}
	if record.Subject != subject || string(record.Data) != tenant {
		return fmt.Errorf("stream returned a record outside the tenant's account")
	}
	return nil
}

func main() {
	if len(os.Args) != 5 {
		os.Exit(2)
	}
	if err := verify(os.Args[1], os.Args[2], os.Args[3], os.Args[4]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
