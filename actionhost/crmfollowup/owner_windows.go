//go:build windows

package crmfollowup

import (
	"errors"
	"io"
	"os"
)

func readHostSecretFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("secret is not a regular non-symbolic-link file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open failed")
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxHostSecretLen+2))
}

// The production action host requires run_as_user and is therefore rejected
// on Windows by config validation. Keep checkout tests portable.
func validateHostCommandPath(string) error { return nil }

func resolveRunAsOwner(string) (int, error)         { return -1, nil }
func validateStageInputDir(os.FileInfo) error       { return nil }
func validateStageInputFile(os.FileInfo, int) error { return nil }
