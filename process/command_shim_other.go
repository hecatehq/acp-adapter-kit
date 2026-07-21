//go:build !windows

package process

import (
	"errors"
	"os/exec"
	"runtime"
)

func validateDirectCommand(string) error {
	return nil
}

func newProcessCommand(_ CommandMode, command string, args []string) *exec.Cmd {
	return exec.Command(command, args...)
}

func resolveWindowsCommandShim(string) (string, error) {
	return "", errors.New("Windows command-shim mode is unavailable on " + runtime.GOOS)
}

func prepareWindowsCommandShim(string, []string, []string) (string, []string, []string, error) {
	return "", nil, nil, errors.New("Windows command-shim mode is unavailable on " + runtime.GOOS)
}
