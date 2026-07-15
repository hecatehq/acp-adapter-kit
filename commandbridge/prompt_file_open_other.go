//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package commandbridge

import (
	"errors"
	"os"
)

func openPromptResource(string) (*os.File, error) {
	return nil, errors.New("secure local file prompt inputs are unsupported on this platform")
}
