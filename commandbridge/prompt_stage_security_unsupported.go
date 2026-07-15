//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package commandbridge

import "errors"

func securePromptResourceStage(string) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}
