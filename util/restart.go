package util

import (
	"os"
	"os/exec"
)

// RestartSelf starts a fresh copy of this process with the same arguments,
// environment and working directory. The copy reclaims the ports this process
// holds, so the caller exits once the copy has started.
func RestartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	return cmd.Start()
}
