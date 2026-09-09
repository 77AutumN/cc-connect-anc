//go:build !windows

package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirHistory_PreservesSupervisorOnlyPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dirHistoryFileName)
	// Model users may traverse the common state parent after multi-user
	// onboarding. Existing histories must not remain readable by them.
	if err := os.WriteFile(path, []byte(`{"owner":["/private/old"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	history := NewDirHistory(dir)
	history.Add("owner", "/private/new")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("directory history permissions = %o, want 600", info.Mode().Perm())
	}
	loaded := NewDirHistory(dir)
	if loaded.Get("owner", 1) != "/private/new" || loaded.Get("owner", 2) != "/private/old" {
		t.Fatal("permission hardening must preserve directory history")
	}
}
