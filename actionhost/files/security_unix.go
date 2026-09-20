//go:build !windows

package files

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

func checkOwner(info os.FileInfo, uid int, single bool) error {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != uid || (single && s.Nlink != 1) {
		return ErrInvalid
	}
	return nil
}

func fileGroup(info os.FileInfo) int {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(s.Gid)
}

func identity(info os.FileInfo) string {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d", s.Dev, s.Ino, s.Uid, s.Gid)
}

func unchanged(a, b os.FileInfo) bool {
	if !os.SameFile(a, b) || a.Size() != b.Size() || a.Mode() != b.Mode() || !a.ModTime().Equal(b.ModTime()) || identity(a) != identity(b) {
		return false
	}
	// ctime catches writes followed by restored mtime; names differ on BSD/macOS.
	x, y := reflect.ValueOf(a.Sys()).Elem(), reflect.ValueOf(b.Sys()).Elem()
	for _, name := range []string{"Ctim", "Ctimespec", "Nlink"} {
		p, q := x.FieldByName(name), y.FieldByName(name)
		if p.IsValid() && (!q.IsValid() || !reflect.DeepEqual(p.Interface(), q.Interface())) {
			return false
		}
	}
	return true
}

// Every ancestor is pinned by a non-replaceable directory. Sticky /tmp is
// accepted only with a host-owned next component, as in the existing CRM host.
func protectedRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrUnavailable
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrUnavailable
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && int(stat.Uid) != os.Geteuid()) {
			return nil, ErrUnavailable
		}
		last := index == len(parts)-1
		if info.Mode().Perm()&0022 != 0 && (last || info.Mode()&os.ModeSticky == 0) {
			return nil, ErrUnavailable
		}
		if last && int(stat.Uid) != os.Geteuid() {
			return nil, ErrUnavailable
		}
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, ErrUnavailable
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = r.Close()
		return nil, ErrUnavailable
	}
	return r, nil
}

func noFollowFlag() int { return syscall.O_NOFOLLOW | syscall.O_NONBLOCK }
