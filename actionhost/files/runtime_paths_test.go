package files

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRuntimePathsRejectWritableOrLinkedLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX ownership")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "launcher.py")
	if err := os.WriteFile(path, []byte("# synthetic candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHostFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if ValidateHostFile(path) == nil {
		t.Fatal("writable launcher accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.py")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if ValidateHostFile(link) == nil || ValidateHostDirectory(link) == nil {
		t.Fatal("linked runtime path accepted")
	}
}
