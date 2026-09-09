//go:build !windows

package teambrain

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func protectedTestCommand(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".team-brain-command-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "host")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandRejectsWritableLinkedAndNonExecutablePaths(t *testing.T) {
	path := protectedTestCommand(t)
	if err := validateCommand(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0600, 0720, 0702} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if validateCommand(path) == nil {
			t.Fatalf("accepted mode %o", mode)
		}
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if validateCommand(link) == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(filepath.Dir(path), 0722); err != nil {
		t.Fatal(err)
	}
	if validateCommand(path) == nil {
		t.Fatal("accepted writable ancestor")
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if validateCommand("relative-command") == nil {
		t.Fatal("accepted relative command")
	}
}

func TestConfigurationRequiresExactlySixUniqueProjectsAndConsumesEnvironment(t *testing.T) {
	path := protectedTestCommand(t)
	for _, projects := range []string{"one", "1,2,3,4,5,5", "1,2,3,4,5, 6", "1,2,3,4,5,6,7", "1,2,3,4,5,6"} {
		t.Setenv("CC_TEAM_BRAIN_COMMAND", path)
		t.Setenv("CC_TEAM_BRAIN_PROJECTS", projects)
		a, err := NewFromEnv()
		valid := projects == "1,2,3,4,5,6"
		if (err == nil) != valid || (a != nil) != valid {
			t.Fatalf("mapping %q = %v", projects, err)
		}
		if os.Getenv("CC_TEAM_BRAIN_COMMAND") != "" || os.Getenv("CC_TEAM_BRAIN_PROJECTS") != "" {
			t.Fatal("host env not consumed")
		}
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if ValidateAgentUser(current.Username) == nil || ValidateAgentUser("root") == nil {
		t.Fatal("accepted privileged account")
	}
}

func TestSafeHostErrorsKeepStageAndCategoryWithoutPrivateText(t *testing.T) {
	message := hostError("bind", safeCode("principal_mismatch")).Error()
	if !strings.Contains(message, "bind") || !strings.Contains(message, "principal_mismatch") {
		t.Fatal("lost safe context")
	}
	if safeCode("/private/secret\n") != "invalid_host_status" {
		t.Fatal("unsafe error text")
	}
}
