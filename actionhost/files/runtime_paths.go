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
