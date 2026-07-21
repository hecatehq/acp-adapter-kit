//go:build windows

package process

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	windowsShimInvocationEnv  = "ACP_ADAPTER_KIT_PROCESS_SHIM_INVOCATION"
	windowsShimReservedPrefix = "ACP_ADAPTER_KIT_PROCESS_SHIM_"
	// The inherited Windows environment block is limited to 32,767 UTF-16
	// code units. Keep the bridge payload bounded so common host environments
	// retain headroom for their ordinary variables.
	windowsShimPayloadMax = 24_000
)

// windowsShimBridgeScript is fixed kit-owned PowerShell. Invocation data is a
// base64 JSON value in one environment variable, never interpolated into this
// script. The bridge removes that variable before starting the command shim so
// the provider and its descendants cannot inherit prompt/config arguments.
const windowsShimBridgeScript = `$ErrorActionPreference = 'Stop'
$encoded = $env:ACP_ADAPTER_KIT_PROCESS_SHIM_INVOCATION
Remove-Item Env:ACP_ADAPTER_KIT_PROCESS_SHIM_INVOCATION -ErrorAction SilentlyContinue
if ([string]::IsNullOrEmpty($encoded)) { throw 'missing command-shim invocation' }
$json = [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($encoded))
$invocation = $json | ConvertFrom-Json
$command = [string]$invocation.command
$arguments = @()
foreach ($argument in $invocation.args) { $arguments += [string]$argument }
& $command @arguments
if ($null -ne $LASTEXITCODE) { exit $LASTEXITCODE }
if (-not $?) { exit 1 }
`

type windowsShimInvocation struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func validateDirectCommand(command string) error {
	ext := strings.ToLower(filepath.Ext(command))
	if ext == ".cmd" || ext == ".bat" {
		return fmt.Errorf("process command %q is a Windows command shim; use CommandModeWindowsCommandShim", command)
	}
	return nil
}

func newProcessCommand(_ CommandMode, command string, args []string) *exec.Cmd {
	return exec.Command(command, args...)
}

func resolveWindowsCommandShim(command string) (string, error) {
	if command == "" {
		return "", errors.New("process command is required")
	}
	if !filepath.IsAbs(command) {
		return "", fmt.Errorf("Windows command shim path must be absolute: %s", command)
	}
	clean := filepath.Clean(command)
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
	raw, err := json.Marshal(windowsShimInvocation{
		Command: command,
		Args:    append([]string(nil), args...),
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("encode Windows command-shim invocation: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if len(encoded) > windowsShimPayloadMax {
		return "", nil, nil, fmt.Errorf("Windows command-shim invocation exceeds %d encoded bytes", windowsShimPayloadMax)
	}

	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locate trusted Windows system directory: %w", err)
	}
	if !filepath.IsAbs(systemDir) {
		return "", nil, nil, fmt.Errorf("trusted Windows system directory is not absolute: %s", systemDir)
	}
	powerShellPath := filepath.Join(systemDir, "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Stat(powerShellPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("inspect trusted Windows PowerShell bridge: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, nil, fmt.Errorf("trusted Windows PowerShell bridge is not a regular file: %s", powerShellPath)
	}

	encodedScript := base64.StdEncoding.EncodeToString(utf16LEBytes(windowsShimBridgeScript))
	launchArgs := []string{
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-EncodedCommand",
		encodedScript,
	}
	return powerShellPath, launchArgs, withWindowsShimInvocation(env, encoded), nil
}

func utf16LEBytes(value string) []byte {
	units := utf16.Encode([]rune(value))
	out := make([]byte, len(units)*2)
	for i, unit := range units {
		out[i*2] = byte(unit)
		out[i*2+1] = byte(unit >> 8)
	}
	return out
}

func withWindowsShimInvocation(env []string, encoded string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			result = append(result, entry)
			continue
		}
		if strings.HasPrefix(strings.ToUpper(name), windowsShimReservedPrefix) {
			continue
		}
		result = append(result, entry)
	}
	result = append(result, windowsShimInvocationEnv+"="+encoded)
	return result
}
