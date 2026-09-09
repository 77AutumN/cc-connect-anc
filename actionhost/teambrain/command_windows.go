//go:build windows

package teambrain

import "errors"

func validateCommand(string) error {
	return errors.New("knowledge production host requires POSIX account isolation")
}
