//go:build !windows

package crmfollowup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func readHostSecretFile(path string) ([]byte, error) {
	info, err := validateTrustedPath(path, false, false)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("secret must be owned by the supervisor with mode 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open failed")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFileIdentity(info, opened) {
		return nil, errors.New("file changed while opening")
	}
	return io.ReadAll(io.LimitReader(file, maxHostSecretLen+2))
}

func validateHostCommandPath(path string) error {
	_, err := validateTrustedPath(path, true, true)
	return err
}

// validateTrustedPath walks every component without following symbolic links.
// Secret files may be owned by root or the supervisor; executable helpers and
// their parent chain must be root-owned. Writable sticky parents (notably
// /tmp in tests) are safe only because the next component must be owned by the
// supervisor/root and is checked again after opening.
func validateTrustedPath(path string, executable, rootOnly bool) (os.FileInfo, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return nil, errors.New("path must be absolute")
	}
	current := string(filepath.Separator)
	parts := splitPathComponents(clean)
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, errors.New("path component is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symbolic links are not allowed")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, errors.New("path ownership is unavailable")
		}
		ownerOK := stat.Uid == 0 || (!rootOnly && int(stat.Uid) == os.Geteuid())
		if !ownerOK {
			return nil, errors.New("path owner is not trusted")
		}
		last := index == len(parts)-1
		if !last {
			if !info.IsDir() {
				return nil, errors.New("parent component is not a directory")
			}
			if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return nil, errors.New("parent directory is replaceable")
			}
			continue
		}
		if !info.Mode().IsRegular() || stat.Nlink != 1 {
			return nil, errors.New("target is not a single-link regular file")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return nil, errors.New("target is writable by group or others")
		}
		if executable && info.Mode().Perm()&0o111 == 0 {
			return nil, errors.New("helper is not executable")
		}
		return info, nil
	}
	return nil, errors.New("empty path")
}

func splitPathComponents(path string) []string {
	volume := filepath.VolumeName(path)
	rest := path[len(volume):]
	var parts []string
	for rest != "" && rest != string(filepath.Separator) {
		rest = filepath.Clean(rest)
		if rest == string(filepath.Separator) || rest == "." {
			break
		}
		dir, base := filepath.Split(rest)
		if base != "" {
			parts = append([]string{base}, parts...)
		}
		rest = filepath.Clean(dir)
	}
	return parts
}

func sameFileIdentity(first, second os.FileInfo) bool {
	a, okA := first.Sys().(*syscall.Stat_t)
	b, okB := second.Sys().(*syscall.Stat_t)
	return okA && okB && a.Dev == b.Dev && a.Ino == b.Ino &&
		a.Uid == b.Uid && a.Mode == b.Mode && a.Nlink == b.Nlink
}

func resolveRunAsOwner(name string) (int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, fmt.Errorf("invalid uid: %w", err)
	}
	return uid, nil
}

func validateStageInputDir(info os.FileInfo) error {
	if info.Mode().Perm() != 0o730 || info.Mode()&os.ModeSetgid == 0 || info.Mode()&os.ModeSticky == 0 {
		return errors.New("CRM stage input directory must have mode 3730")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("CRM stage input directory is not owned by cc-connect")
	}
	return nil
}

func validateStageInputFile(info os.FileInfo, wantUID int) error {
	if info.Mode().Perm() != 0o640 {
		return errors.New("CRM stage input must have mode 0640")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != wantUID {
		return errors.New("CRM stage input owner does not match run_as_user")
	}
	if stat.Nlink != 1 {
		return errors.New("CRM stage input must have exactly one link")
	}
	return nil
}
