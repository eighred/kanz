package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeRunner captures the command sshJoiner would run, so we can assert the k3s
// install line without a network.
type fakeRunner struct {
	gotCmd string
	out    string
	err    error
}

func (f *fakeRunner) run(_ context.Context, _, _ string, _ []byte, cmd string) (string, error) {
	f.gotCmd = cmd
	return f.out, f.err
}

func TestSSHJoinerRunsK3sInstall(t *testing.T) {
	fr := &fakeRunner{out: "ok"}
	j := &sshJoiner{run: fr.run}
	err := j.Join(context.Background(),
		Target{Addr: "10.0.0.5:22", User: "root", Key: []byte("k")},
		K3sJoin{ServerURL: "https://cp:6443", Token: "tok"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	for _, want := range []string{"get.k3s.io", "K3S_URL='https://cp:6443'", "K3S_TOKEN='tok'", "agent"} {
		if !strings.Contains(fr.gotCmd, want) {
			t.Errorf("install cmd %q missing %q", fr.gotCmd, want)
		}
	}
}

func TestSSHJoinerPropagatesFailure(t *testing.T) {
	fr := &fakeRunner{err: errors.New("dial refused")}
	j := &sshJoiner{run: fr.run}
	if err := j.Join(context.Background(), Target{Addr: "x:22", User: "root", Key: []byte("k")}, K3sJoin{}); err == nil {
		t.Fatal("expected the runner error to propagate")
	}
}

type fakeJoiner struct {
	called bool
	err    error
}

func (f *fakeJoiner) Join(context.Context, Target, K3sJoin) error {
	f.called = true
	return f.err
}

func TestRunRequiresEnv(t *testing.T) {
	t.Setenv("PROVISION_TARGET_ADDR", "")
	if err := run(&fakeJoiner{}); err == nil {
		t.Fatal("expected an error when required env is missing")
	}
}

func TestRunReadsKeyAndJoins(t *testing.T) {
	dir := t.TempDir()
	keyFile := dir + "/key"
	if err := os.WriteFile(keyFile, []byte("PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROVISION_TARGET_ADDR", "10.0.0.5:22")
	t.Setenv("PROVISION_SSH_USER", "root")
	t.Setenv("K3S_SERVER_URL", "https://cp:6443")
	t.Setenv("K3S_TOKEN", "tok")
	t.Setenv("PROVISION_SSH_KEY_FILE", keyFile)

	fj := &fakeJoiner{}
	if err := run(fj); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !fj.called {
		t.Fatal("expected Join to be called")
	}
}
