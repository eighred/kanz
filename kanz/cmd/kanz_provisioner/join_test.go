package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner captures the command sshJoiner would run, so we can assert the k3s
// install line without a network.
type fakeRunner struct {
	gotCmd string
	err    error
}

func (f *fakeRunner) run(_ context.Context, _, _ string, _ []byte, cmd string) error {
	f.gotCmd = cmd
	return f.err
}

func TestSSHJoinerRunsK3sInstall(t *testing.T) {
	fr := &fakeRunner{}
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
	err := j.Join(context.Background(), Target{Addr: "x:22", User: "root", Key: []byte("k")}, K3sJoin{})
	if !errors.Is(err, fr.err) {
		t.Fatal("the install error must retain the local cause")
	}
}

func TestInstallScriptRequiresCompleteDownload(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell required to exercise the remote install script")
	}
	dir := t.TempDir()
	// Exercise the remote script without networking. Even a downloader that
	// writes executable content before failing must never start the installer.
	curl := `#!/bin/sh
while [ "$1" != "--output" ]; do shift; done
if [ "$MODE" != "empty" ]; then
  cat > "$2" <<'INSTALL'
printf '%s\n' "$K3S_URL" "$K3S_TOKEN" "$1" > observed
INSTALL
fi
[ "$MODE" != "failed" ]
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o700); err != nil {
		t.Fatal(err)
	}
	join := K3sJoin{ServerURL: "https://fixture.invalid:6443", Token: "sample' quoted value"}
	for _, mode := range []string{"failed", "empty", "complete"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(sh, "-s")
			command.Dir = dir
			command.Env = append(os.Environ(), "MODE="+mode)
			// A relative path is portable across POSIX and Windows Git Bash.
			command.Stdin = strings.NewReader("PATH=.:$PATH\n" + k3sInstallScript(join))
			err := command.Run()
			got, readErr := os.ReadFile(filepath.Join(dir, "observed"))
			if mode != "complete" {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatal("download failure or absence must prevent installer execution")
				}
				return
			}
			if err != nil || readErr != nil || string(got) != join.ServerURL+"\n"+join.Token+"\nagent\n" {
				t.Fatal("complete installer must receive the exact configured values and agent mode")
			}
		})
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
