package files

import (
	"os"
	"path/filepath"
)

// PrepareWork is host-only. The protected base is provisioned explicitly; only
// a session-derived child is created. Existing directories are verified, never
// repaired or reassigned. The launcher still must isolate each model's reads.
func PrepareWork(base, sessionID string, ownerUID int) (string, error) {
	if sessionID == "" || ownerUID < 0 {
		return "", ErrInvalid
	}
	r, err := protectedRoot(base)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	name := hash([]byte(sessionID))
	_, err = r.Lstat(name)
	if os.IsNotExist(err) {
		if err = newDirectory(r, name, 0755, os.Geteuid()); err != nil {
			return "", err
		}
		child, err := r.OpenRoot(name)
		if err != nil {
			return "", ErrUnavailable
		}
		defer func() { _ = child.Close() }()
		if err = newDirectory(child, "inputs", 0755, os.Geteuid()); err != nil {
			return "", err
		}
		if err = newDirectory(child, "outputs", 0700, ownerUID); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", ErrUnavailable
	}
	workPath := filepath.Join(base, name)
	w, err := protectedRoot(workPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = w.Close() }()
	for _, item := range []struct {
		name string
		mode os.FileMode
		uid  int
	}{{".", 0755, os.Geteuid()}, {"inputs", 0755, os.Geteuid()}, {"outputs", 0700, ownerUID}} {
		info, err := w.Lstat(item.name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != item.mode || checkOwner(info, item.uid, false) != nil {
			return "", ErrUnavailable
		}
	}
	return workPath, nil
}

func newDirectory(root *os.Root, name string, mode os.FileMode, uid int) error {
	if err := root.Mkdir(name, 0700); err != nil {
		return ErrUnavailable
	}
	f, err := root.Open(name)
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = f.Close() }()
	if uid != os.Geteuid() {
		if err = f.Chown(uid, -1); err != nil {
			return ErrUnavailable
		}
	}
	if err = f.Chmod(mode); err != nil {
		return ErrUnavailable
	}
	return nil
}
