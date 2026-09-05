package claudecode

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type sessionValidationSudo func(context.Context, ...string) ([]byte, error)

func (f sessionValidationSudo) Run(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}

func TestValidateSessionIDRunAsUsesNativeUserHomeAndExactWorkspace(t *testing.T) {
	supervisor, nativeHome, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", supervisor)
	t.Setenv("USERPROFILE", supervisor)
	const nativeUser, id = "fixture-claude", "original-session-id"
	lookup := func(name string) (*user.User, error) {
		if name != nativeUser {
			t.Fatal("validator did not resolve the configured runtime user")
		}
		return &user.User{Username: name, HomeDir: nativeHome}, nil
	}
	want := []string{"-n", "-iu", nativeUser, "--", "/usr/bin/test", "-f",
		filepath.Join(nativeHome, ".claude", "projects", encodeClaudeProjectKey(workspace), id+".jsonl")}
	calls := 0
	runner := sessionValidationSudo(func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
			t.Error("runtime-user validation must have one bounded deadline")
		}
		if !reflect.DeepEqual(args, want) {
			return nil, errors.New("session absent in this exact workspace")
		}
		return nil, nil // Only the native user can see this synthetic session.
	})
	a := &Agent{workDir: workspace, spawnOpts: core.SpawnOptions{RunAsUser: nativeUser}}
	if !a.validateSessionID(context.Background(), id, runner, lookup) || calls != 1 {
		t.Fatal("valid native-user session was lost because the supervisor has no Claude project store")
	}
	a.workDir = t.TempDir()
	if a.validateSessionID(context.Background(), id, runner, lookup) {
		t.Fatal("runtime-user lookup crossed the configured workspace boundary")
	}
	if os.Getenv("HOME") != supervisor || os.Getenv("USERPROFILE") != supervisor {
		t.Fatal("validator changed the supervisor home")
	}
}

func TestValidateSessionIDRunAsDoesNotTrustSupervisorCopy(t *testing.T) {
	supervisor, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", supervisor)
	t.Setenv("USERPROFILE", supervisor)
	const id = "supervisor-only-session"
	project := filepath.Join(supervisor, ".claude", "projects", encodeClaudeProjectKey(workspace))
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, id+".jsonl"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{workDir: workspace, spawnOpts: core.SpawnOptions{RunAsUser: "fixture-claude"}}
	lookup := func(string) (*user.User, error) { return &user.User{HomeDir: t.TempDir()}, nil }
	runner := sessionValidationSudo(func(context.Context, ...string) ([]byte, error) {
		return []byte("private native-user diagnostic must be ignored"), context.DeadlineExceeded
	})
	if a.validateSessionID(context.Background(), id, runner, lookup) {
		t.Fatal("supervisor session file overrode failed native-user validation")
	}
}

func TestValidateSessionIDRunAsRejectsUntrustedIDsAndLookupFailure(t *testing.T) {
	a := &Agent{workDir: t.TempDir(), spawnOpts: core.SpawnOptions{RunAsUser: "fixture-claude"}}
	calls := 0
	runner := sessionValidationSudo(func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, nil
	})
	lookups := 0
	lookup := func(string) (*user.User, error) {
		lookups++
		return nil, errors.New("unknown OS user")
	}
	for _, id := range []string{"", "../escape", `..\escape`, "/absolute", ".", "..", "id;command", "id\ncommand", strings.Repeat("x", 129)} {
		if a.validateSessionID(context.Background(), id, runner, lookup) {
			t.Errorf("invalid ID accepted: %q", id)
		}
	}
	if lookups != 0 || calls != 0 {
		t.Fatal("invalid session ID reached OS lookup or privileged execution")
	}
	if a.validateSessionID(context.Background(), "valid-id", runner, lookup) || lookups != 1 || calls != 0 {
		t.Fatal("invalid ID or unknown user reached privileged execution")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a.validateSessionID(ctx, "valid-id", runner, lookup) || lookups != 1 || calls != 0 {
		t.Fatal("cancelled validation performed OS work")
	}
}

func TestValidateSessionIDWithoutRunAsPreservesSupervisorStore(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const id = "legacy-supervisor-session"
	project := filepath.Join(home, ".claude", "projects", encodeClaudeProjectKey(workspace))
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, id+".jsonl"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{workDir: workspace}
	if !a.ValidateSessionID(context.Background(), id) {
		t.Fatal("public validator rejected the existing non-isolated supervisor session")
	}
	runner := sessionValidationSudo(func(context.Context, ...string) ([]byte, error) {
		t.Fatal("non-isolated validation called sudo")
		return nil, nil
	})
	lookup := func(string) (*user.User, error) {
		t.Fatal("non-isolated validation looked up another OS user")
		return nil, nil
	}
	if !a.validateSessionID(context.Background(), id, runner, lookup) {
		t.Fatal("shared helper changed legacy supervisor validation")
	}
}
