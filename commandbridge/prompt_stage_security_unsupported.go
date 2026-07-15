//go:build !darwin && !linux && !windows

package commandbridge

import "errors"

type unsupportedPromptResourceStageGuard struct{}

func createPromptResourceStage(string) (string, string, promptResourceStageGuard, error) {
	return "", "", nil, errors.New("secure rich prompt input staging is unsupported on this platform")
}

func preparePromptResourceParent(string) (string, error) {
	return "", errors.New("secure rich prompt input staging is unsupported on this platform")
}

func newPromptResourceStageGuard(string, string) (promptResourceStageGuard, error) {
	return nil, errors.New("secure rich prompt input staging is unsupported on this platform")
}

func (*unsupportedPromptResourceStageGuard) Verify() error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func (*unsupportedPromptResourceStageGuard) Secure() error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func (*unsupportedPromptResourceStageGuard) ProtectFile(string) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func (*unsupportedPromptResourceStageGuard) Seal() error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func (*unsupportedPromptResourceStageGuard) Cleanup(func(string) error) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func securePromptResourceStage(string) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}

func verifyPromptResourceStageSecurity(string) error {
	return errors.New("secure rich prompt input staging is unsupported on this platform")
}
