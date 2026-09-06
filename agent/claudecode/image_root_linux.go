package claudecode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux service path: walk from an anchored directory descriptor, rejecting
// symlinks atomically. A preflight Lstat followed by OpenRoot would race a rename
// of a Claude-owned ancestor. mkdirat also stays under the checked descriptor.
func openImageCacheRoot(path string, create bool) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		if part == "" {
			continue
		}
		flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		next, openErr := unix.Openat(fd, part, flags, 0)
		created := false
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(fd, part, 0750)
			created = mkdirErr == nil
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return nil, mkdirErr
			}
			next, openErr = unix.Openat(fd, part, flags, 0)
		}
		if openErr != nil {
			return nil, &os.PathError{Op: "open image directory", Path: part, Err: openErr}
		}
		unix.Close(fd)
		fd = next
		if created {
			if err := unix.Fchmod(fd, 0750); err != nil {
				return nil, err
			}
		}
	}
	// /proc/self/fd refers to this still-open descriptor, not the mutable path.
	return os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
}
