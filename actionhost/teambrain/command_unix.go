//go:build !windows

package teambrain

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func validateCommand(command string) error {
	if !filepath.IsAbs(command) {
		return errors.New("knowledge command must be absolute")
	}
	for current := command; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return errors.New("knowledge command unavailable")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (int(owner.Uid) != os.Geteuid() && owner.Uid != 0) || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("unsafe knowledge command ownership or permissions")
		}
		if current == command && (!info.Mode().IsRegular() || info.Mode().Perm()&0100 == 0) {
			return errors.New("knowledge command is not executable")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
