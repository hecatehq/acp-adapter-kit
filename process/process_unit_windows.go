//go:build windows

package process

import "os/exec"

// os/exec does not expose a race-free pre-start Job Object assignment. Keep
// CommandContext's immediate-child cancellation and rely on WaitDelay to bound
// inherited pipe handles on Windows.
func configureProcessUnit(_ *exec.Cmd) {}

func cancelProcessUnit(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
