//go:build windows

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	windowsShimCommandEnv = "ACP_ADAPTER_KIT_PROCESS_SHIM_COMMAND"
	// cmd.exe accepts at most 8191 expanded command-line characters. Keep a
	// margin for fixed switches and fail before the payload reaches that bound.
	windowsCommandMax = 8000
)

func validateDirectCommand(command string) error {
	ext := strings.ToLower(filepath.Ext(command))
	if ext == ".cmd" || ext == ".bat" {
		return fmt.Errorf("process command %q is a Windows command shim; use CommandModeWindowsCommandShim", command)
	}
	return nil
}

func newProcessCommand(mode CommandMode, command string, args []string) *exec.Cmd {
	if mode != CommandModeWindowsCommandShim {
		return exec.Command(command, args...)
	}
	cmd := exec.Command(command)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// cmd.exe does not use CommandLineToArgvW parsing. Bypass os/exec's
		// generic Windows argv quoting and provide the exact command line,
		// which contains only fixed switches and kit-owned variable names.
		CmdLine: syscall.EscapeArg(command) + " " + strings.Join(args, " "),
	}
	return cmd
}

func resolveWindowsCommandShim(command string) (string, error) {
	if command == "" {
		return "", errors.New("process command is required")
	}
	if !filepath.IsAbs(command) {
		return "", fmt.Errorf("Windows command shim path must be absolute: %s", command)
	}
	clean := filepath.Clean(command)
	if err := validateWindowsShimValue("path", clean, false); err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if ext != ".cmd" && ext != ".bat" {
		return "", fmt.Errorf("Windows command shim must have a .cmd or .bat extension: %s", clean)
	}
	info, err := os.Stat(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &CommandNotFoundError{Command: command, Err: err}
		}
		return "", fmt.Errorf("stat Windows command shim %q: %w", clean, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Windows command shim is not a regular file: %s", clean)
	}
	return clean, nil
}

func prepareWindowsCommandShim(command string, args []string, env []string) (string, []string, []string, error) {
	for i, arg := range args {
		if err := validateWindowsShimValue(fmt.Sprintf("arg %d", i), arg, true); err != nil {
			return "", nil, nil, err
		}
	}

	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locate trusted Windows system directory: %w", err)
	}
	if !filepath.IsAbs(systemDir) {
		return "", nil, nil, fmt.Errorf("trusted Windows system directory is not absolute: %s", systemDir)
	}
	cmdPath := filepath.Join(systemDir, "cmd.exe")

	shimEnv := make(map[string]string, len(args)+1)
	shimEnv[windowsShimCommandEnv] = command
	references := make([]string, 0, len(args)+1)
	references = append(references, `"%`+windowsShimCommandEnv+`%"`)
	expandedLength := len(command) + 4 // command quotes plus outer /s quotes
	for i, arg := range args {
		name := fmt.Sprintf("ACP_ADAPTER_KIT_PROCESS_SHIM_ARG_%04d", i)
		shimEnv[name] = arg
		references = append(references, `"%`+name+`%"`)
		expandedLength += len(arg) + 3 // separating space plus argument quotes
	}
	payload := `"` + strings.Join(references, " ") + `"`
	launchArgs := []string{"/d", "/e:on", "/v:off", "/s", "/c", payload}
	fixedLength := len(syscall.EscapeArg(cmdPath)) + 1 + len(strings.Join(launchArgs[:5], " ")) + 1
	if fixedLength+len(payload) > windowsCommandMax || fixedLength+expandedLength > windowsCommandMax {
		return "", nil, nil, fmt.Errorf("Windows command shim invocation exceeds %d characters", windowsCommandMax)
	}

	// Standard package-manager shims depend on command extensions for SETLOCAL,
	// CALL :label, and %~dp0. Enable them deterministically while disabling
	// AutoRun and delayed expansion; no user-derived byte enters the raw line.
	return cmdPath,
		launchArgs,
		withWindowsEnvOverrides(env, shimEnv),
		nil
}

func validateWindowsShimValue(label string, value string, emptyAllowed bool) error {
	if !emptyAllowed && value == "" {
		return fmt.Errorf("Windows command shim %s is required", label)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("Windows command shim %s has leading or trailing whitespace", label)
	}
	if strings.ContainsAny(value, "\x00\r\n\"%!^&|<>()") {
		return fmt.Errorf("Windows command shim %s contains unsafe command-interpreter characters", label)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("Windows command shim %s contains unsafe control characters", label)
		}
	}
	return nil
}

func withWindowsEnvOverrides(env []string, overrides map[string]string) []string {
	result := make([]string, 0, len(env)+len(overrides))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !containsEnvNameFold(overrides, name) {
			result = append(result, entry)
		}
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+overrides[name])
	}
	return result
}

func containsEnvNameFold(values map[string]string, candidate string) bool {
	for name := range values {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}
