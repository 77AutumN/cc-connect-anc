package files

import (
	"os"
	"path/filepath"
)

// ValidateHostDirectory never creates or repairs deployment configuration.
func ValidateHostDirectory(path string) error {
	r, err := protectedRoot(path)
	if err != nil {
		return err
	}
	return r.Close()
}

// The dedicated model group is inherited by new work directories. Only the
// gateway and this project's model identity should be members; runtime setup is
// explicit and this check never changes memberships or existing modes.
func ValidateWorkGroup(path string, gid int) error {
	if gid <= 0 {
		return ErrUnavailable
	}
	r, err := protectedRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	info, err := r.Stat(".")
	if err != nil || fileGroup(info) != gid || info.Mode().Perm() != 0750 {
		return ErrUnavailable
	}
	groups, err := os.Getgroups()
	if err != nil {
		return ErrUnavailable
	}
	for _, group := range groups {
		if group == gid {
			return nil
		}
	}
	if os.Getegid() == gid {
		return nil
	}
	return ErrUnavailable
}

func ValidateHostFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnavailable
	}
	r, err := protectedRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	f, err := r.Lstat(filepath.Base(path))
	if err != nil || !f.Mode().IsRegular() || f.Mode().Perm()&0022 != 0 || checkOwner(f, os.Geteuid(), true) != nil {
		return ErrUnavailable
	}
	return nil
}
