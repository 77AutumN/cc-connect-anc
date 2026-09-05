//go:build windows

package main

import (
	"os"
	"os/exec"
)

func restartProcess(execPath string, env []string) error {
	cmd := exec.Command(execPath, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env
	return cmd.Start()
}
