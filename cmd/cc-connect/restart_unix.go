//go:build !windows

package main

import (
	"os"
	"syscall"
)

func restartProcess(execPath string, env []string) error {
	return syscall.Exec(execPath, os.Args, env)
}
