//go:build windows

package process_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

func TestWindowsCommandShimRun(t *testing.T) {
	shim := writeWindowsCommandShim(t, "run.cmd", `@echo off
if defined ACP_ADAPTER_KIT_PROCESS_SHIM_INVOCATION exit /b 40
if defined ACP_ADAPTER_KIT_PROCESS_SHIM_COMMAND exit /b 43
if defined ACP_ADAPTER_KIT_PROCESS_SHIM_ARG_0000 exit /b 44
if not "%~1"=="hello world" exit /b 41
if not "%~2"=="--flag=value" exit /b 42
echo RUN:%~1:%~2:%VISIBLE%
`)
	args := []string{"hello world", "--flag=value"}
	result, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        args,
		Dir:         t.TempDir(),
		Env: adapterprocess.EnvPolicy{Set: map[string]string{
			"VISIBLE":                               "yes",
			"acp_adapter_kit_process_shim_command":  "C:\\malicious&ignored.cmd",
			"ACP_ADAPTER_KIT_PROCESS_SHIM_ARG_0000": "malicious&ignored",
		}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v; stderr=%s", err, result.Stderr)
	}
	if got, want := strings.TrimSpace(string(result.Stdout)), "RUN:hello world:--flag=value:yes"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if result.Command != shim {
		t.Fatalf("result command = %q, want shim %q", result.Command, shim)
	}
	if !reflect.DeepEqual(result.Args, args) {
		t.Fatalf("result args = %#v, want %#v", result.Args, args)
	}
}

func TestWindowsCommandShimRunWithHostOwnedBaseEnvironment(t *testing.T) {
	t.Setenv("HOST_SECRET", "must-not-leak")
	shim := writeWindowsCommandShim(t, "base-env.cmd", `@echo off
if defined HOST_SECRET echo LEAK:%HOST_SECRET%
echo BASE:%VISIBLE%
`)
	result, err := adapterprocess.RunWithBaseEnv(context.Background(), adapterprocess.Spec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Dir:         t.TempDir(),
		Env: adapterprocess.EnvPolicy{
			Inherit: []string{"VISIBLE", "HOST_SECRET"},
		},
	}, []string{"VISIBLE=from-base"})
	if err != nil {
		t.Fatalf("RunWithBaseEnv returned error: %v; stderr=%s", err, result.Stderr)
	}
	if got, want := strings.TrimSpace(string(result.Stdout)), "BASE:from-base"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWindowsCommandShimRunStream(t *testing.T) {
	shim := writeWindowsCommandShim(t, "stream.bat", `@echo off
echo first:%~1
echo second:%~2
`)
	var streamed strings.Builder
	result, err := adapterprocess.RunStream(context.Background(), adapterprocess.Spec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        []string{"one", "two words"},
		Dir:         t.TempDir(),
	}, func(chunk []byte) error {
		streamed.Write(chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStream returned error: %v; stderr=%s", err, result.Stderr)
	}
	if got := strings.ReplaceAll(streamed.String(), "\r\n", "\n"); got != "first:one\nsecond:two words\n" {
		t.Fatalf("streamed stdout = %q", got)
	}
	if streamed.String() != string(result.Stdout) {
		t.Fatalf("streamed stdout = %q, captured stdout = %q", streamed.String(), result.Stdout)
	}
}

func TestWindowsCommandShimSupportsPackageManagerWrapper(t *testing.T) {
	shim := writeWindowsCommandShim(t, "package-manager.cmd", `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0

IF EXIST "%dp0%\node.exe" (
  SET "_prog=%dp0%\node.exe"
) ELSE (
  SET "_prog=node"
  SET PATHEXT=%PATHEXT:;.JS;=;%
)

endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%" "-test.run=TestWindowsNPMShimHelper" "--" %*
`)
	copyWindowsTestExecutable(t, filepath.Join(filepath.Dir(shim), "node.exe"))
	args := []string{
		`prompt with "quotes" & (parentheses)`,
		`100% !important! ^ caret | pipe <in>`,
		` leading and trailing `,
		`{"mcp":{"headers":{"Authorization":"Bearer secret"}}}`,
	}
	child, err := adapterprocess.StartWithBaseEnv(context.Background(), adapterprocess.StartSpec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        args,
		Dir:         t.TempDir(),
		Env: adapterprocess.EnvPolicy{Set: map[string]string{
			"GO_WANT_WINDOWS_NPM_SHIM_HELPER": "1",
		}},
	}, []string{})
	if err != nil {
		t.Fatalf("StartWithBaseEnv returned error: %v", err)
	}
	if _, err := io.WriteString(child.Stdin, "npm wrapper stdin"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := child.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	stdout, err := io.ReadAll(child.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	var exitErr *adapterprocess.ExitError
	if err := child.Wait(); !errors.As(err, &exitErr) || exitErr.Code != 23 {
		t.Fatalf("Wait error = %T %[1]v, want exit code 23; stderr=%s", err, child.Stderr())
	}
	normalized := strings.ReplaceAll(string(stdout), "\r\n", "\n")
	wantArgsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal expected args: %v", err)
	}
	if !strings.Contains(normalized, "NPM_ARGS_JSON="+string(wantArgsJSON)+"\n") {
		t.Fatalf("stdout = %q, want exact forwarded %%* arguments %s", normalized, wantArgsJSON)
	}
	if !strings.Contains(normalized, "NPM_STDIN=npm wrapper stdin\n") {
		t.Fatalf("stdout = %q, want forwarded stdin", normalized)
	}
}

func TestWindowsNPMShimHelper(t *testing.T) {
	if os.Getenv("GO_WANT_WINDOWS_NPM_SHIM_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 {
		os.Exit(21)
	}
	stdin, _ := io.ReadAll(os.Stdin)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "ACP_ADAPTER_KIT_PROCESS_SHIM_") {
			fmt.Printf("NPM_SHIM_ENV_LEAK=%s\n", name)
			os.Exit(24)
		}
	}
	rawArgs, _ := json.Marshal(os.Args[separator+1:])
	fmt.Printf("NPM_ARGS_JSON=%s\n", rawArgs)
	fmt.Printf("NPM_STDIN=%s\n", stdin)
	os.Exit(23)
}

func copyWindowsTestExecutable(t *testing.T, destination string) {
	t.Helper()
	source, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatalf("open Windows test executable: %v", err)
	}
	defer source.Close()
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatalf("create sibling package-manager executable: %v", err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatalf("copy sibling package-manager executable: %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("close sibling package-manager executable: %v", err)
	}
}

func TestWindowsCommandShimStart(t *testing.T) {
	shim := writeWindowsCommandShim(t, "start.cmd", `@echo off
set /p INPUT=
echo START:%~1:%INPUT%
`)
	child, err := adapterprocess.Start(context.Background(), adapterprocess.StartSpec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        []string{"session"},
		Dir:         t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if child.Command != shim {
		t.Fatalf("child command = %q, want shim %q", child.Command, shim)
	}
	if _, err := io.WriteString(child.Stdin, "prompt text\r\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := child.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	stdout, err := io.ReadAll(child.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v; stderr=%s", err, child.Stderr())
	}
	if got, want := strings.TrimSpace(string(stdout)), "START:session:prompt text"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWindowsCommandShimRejectsOnlyUnrepresentableArgument(t *testing.T) {
	shim := writeWindowsCommandShim(t, "safe & agent.cmd", "@echo off\r\necho safe\r\n")
	result, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        []string{`quotes " percent% bang! caret^ ampersand& pipe| redirect<> parentheses()`, " leading and trailing "},
		Dir:         t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run error = %v; stderr=%s", err, result.Stderr)
	}

	_, err = adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        []string{"nul\x00byte"},
		Dir:         t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "process arg 0 contains NUL byte") {
		t.Fatalf("Run error = %v, want NUL rejection", err)
	}
}

func TestWindowsCommandShimRejectsOversizedEncodedInvocation(t *testing.T) {
	shim := writeWindowsCommandShim(t, "safe.cmd", "@echo off\r\necho safe\r\n")
	_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     shim,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Args:        []string{strings.Repeat("a", 18_001)},
		Dir:         t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "invocation exceeds 24000 encoded bytes") {
		t.Fatalf("Run error = %v, want encoded-invocation limit", err)
	}
}

func TestWindowsCommandShimRequiresExplicitModeAndSupportedTarget(t *testing.T) {
	shim := writeWindowsCommandShim(t, "explicit.cmd", "@echo off\r\necho safe\r\n")
	_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command: shim,
		Dir:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "use CommandModeWindowsCommandShim") {
		t.Fatalf("Run error = %v, want explicit-mode rejection", err)
	}

	unsupported := filepath.Join(t.TempDir(), "agent.ps1")
	if err := os.WriteFile(unsupported, []byte("Write-Output safe\r\n"), 0o600); err != nil {
		t.Fatalf("write unsupported script: %v", err)
	}
	_, err = adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     unsupported,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Dir:         t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "must have a .cmd or .bat extension") {
		t.Fatalf("Run error = %v, want extension rejection", err)
	}

	_, err = adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     "relative.cmd",
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Dir:         t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "path must be absolute") {
		t.Fatalf("Run error = %v, want absolute-path rejection", err)
	}

	directory := filepath.Join(t.TempDir(), "directory.cmd")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create shim-shaped directory: %v", err)
	}
	_, err = adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     directory,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Dir:         t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("Run error = %v, want regular-file rejection", err)
	}
}

func TestWindowsCommandShimMissingTargetReturnsTypedError(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing.cmd")
	_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command:     missingPath,
		CommandMode: adapterprocess.CommandModeWindowsCommandShim,
		Dir:         t.TempDir(),
	})
	var missing *adapterprocess.CommandNotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %T %[1]v, want CommandNotFoundError", err)
	}
	if missing.Command != missingPath {
		t.Fatalf("missing command = %q, want %q", missing.Command, missingPath)
	}
}

func TestWindowsCommandShimStillRejectsDirectShell(t *testing.T) {
	_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
		Command: `C:\Windows\System32\cmd.exe`,
		Dir:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "is a shell") {
		t.Fatalf("Run error = %v, want direct shell rejection", err)
	}
}

func writeWindowsCommandShim(t *testing.T, name string, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	contents = strings.ReplaceAll(strings.ReplaceAll(contents, "\r\n", "\n"), "\n", "\r\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write Windows command shim: %v", err)
	}
	return path
}

func TestContextCancellationKillsDescendantJob(t *testing.T) {
	mutationPath := filepath.Join(t.TempDir(), "late-windows-mutation")
	readyPath := filepath.Join(t.TempDir(), "windows-descendant-ready")
	command, args := helperCommand("spawn-descendant", mutationPath, readyPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := adapterprocess.Run(ctx, adapterprocess.Spec{
			Command: command,
			Args:    args,
			Dir:     t.TempDir(),
			Env: adapterprocess.EnvPolicy{Set: map[string]string{
				"GO_WANT_PROCESS_HELPER": "1",
			}},
		})
		done <- err
	}()
	waitForWindowsProcessHelperFile(t, readyPath)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}

	time.Sleep(650 * time.Millisecond)
	if _, statErr := os.Stat(mutationPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant mutated workspace after Windows job cancellation: %v", statErr)
	}
}

func waitForWindowsProcessHelperFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat process helper signal: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process helper did not signal readiness at %s", path)
}
