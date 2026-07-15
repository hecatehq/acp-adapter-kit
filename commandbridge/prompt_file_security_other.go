//go:build !darwin && !linux && !windows

package commandbridge

import (
	"errors"
	"os"
)

func securePrivatePromptResourceFile(*os.File, os.FileMode) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func verifyPrivatePromptResourceFile(*os.File) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}
