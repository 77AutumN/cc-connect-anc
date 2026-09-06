//go:build !windows

package crmfollowup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateStageInputFileRequiresExactModeAndSingleLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStageInputFile(info, os.Geteuid()); err != nil {
		t.Fatalf("valid stage input rejected: %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStageInputFile(info, os.Geteuid()); err == nil {
		t.Fatal("mode 0600 stage input was accepted")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStageInputFile(info, os.Geteuid()); err == nil {
		t.Fatal("multiply-linked stage input was accepted")
	}
}

func TestValidateStageInputDirRequiresSupervisorOwnerAnd3730(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, os.FileMode(0o730)|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStageInputDir(info); err != nil {
		t.Fatalf("valid exchange directory rejected: %v", err)
	}

	if err := os.Chmod(dir, 0o730); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStageInputDir(info); err == nil {
		t.Fatal("exchange directory without setgid+sticky was accepted")
	}
}
