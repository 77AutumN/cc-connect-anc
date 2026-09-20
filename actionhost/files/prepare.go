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
	baseInfo, err := r.Stat(".")
	if err != nil || fileGroup(baseInfo) < 0 {
		return "", ErrUnavailable
	}
	gid := fileGroup(baseInfo)
	name := hash([]byte(sessionID))
	_, err = r.Lstat(name)
	if os.IsNotExist(err) {
		if err = newDirectory(r, name, os.ModeSetgid|0750, gid); err != nil {
			return "", err
		}
		child, err := r.OpenRoot(name)
		if err != nil {
			return "", ErrUnavailable
		}
		defer func() { _ = child.Close() }()
		if err = newDirectory(child, "inputs", os.ModeSetgid|0750, gid); err != nil {
			return "", err
		}
		if err = newDirectory(child, "outputs", os.ModeSetgid|0770, gid); err != nil {
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
	}{{".", os.ModeSetgid | 0750}, {"inputs", os.ModeSetgid | 0750}, {"outputs", os.ModeSetgid | 0770}} {
		info, err := w.Lstat(item.name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&(os.ModeSetgid|os.ModePerm) != item.mode || checkOwner(info, os.Geteuid(), false) != nil || fileGroup(info) != gid {
			return "", ErrUnavailable
		}
	}
	return workPath, nil
}

func newDirectory(root *os.Root, name string, mode os.FileMode, gid int) error {
	if err := root.Mkdir(name, 0700); err != nil {
		return ErrUnavailable
	}
	f, err := root.Open(name)
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ErrUnavailable
	}
	// Ordinary owners may select a group they already belong to. No UID chown
	// or DAC capability is needed; the operator provisions membership explicitly.
	if fileGroup(info) != gid {
		if err = f.Chown(-1, gid); err != nil {
			return ErrUnavailable
		}
	}
	if err = f.Chmod(mode); err != nil {
		return ErrUnavailable
	}
	return nil
}
