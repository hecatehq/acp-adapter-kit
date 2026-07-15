//go:build !windows

package commandbridge

import "os"

func verifyPrivatePromptResourceFile(*os.File) error {
	return nil
}
