//go:build !linux

package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// open rejects symlinks in every path component before opening a rooted handle.
// Production provisions only the final directory writable by the gateway; it
// must not grant write access to the workspace or change its global umask.
func openImageCacheRoot(path string, create bool) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) && create {
			err = os.Mkdir(current, 0750)
			if err == nil {
				err = os.Chmod(current, 0750)
			}
			if err != nil && !os.IsExist(err) {
				return nil, err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("unsafe image cache directory")
		}
	}
	return os.OpenRoot(abs)
}
