//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package commandbridge

import "os"

func openPromptResource(path string) (*os.File, error) {
	return os.Open(path)
}
