//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package commandbridge

import "os"

func securePromptResourceStage(path string) error {
	return os.Chmod(path, 0o700)
}
