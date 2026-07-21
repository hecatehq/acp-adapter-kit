//go:build !windows

package process_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

func TestContextCancellationKillsDescendantProcessGroup(t *testing.T) {
	mutationPath := filepath.Join(t.TempDir(), "late-mutation")
	readyPath := filepath.Join(t.TempDir(), "descendant-ready")
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
	waitForProcessHelperFile(t, readyPath)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}

	time.Sleep(650 * time.Millisecond)
	if _, statErr := os.Stat(mutationPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant mutated workspace after cancellation: %v", statErr)
	}
}

func TestWindowsCommandShimModeIsRejectedOffWindows(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "agent.cmd")
	workDir := t.TempDir()
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "run",
			run: func() error {
				_, err := adapterprocess.Run(context.Background(), adapterprocess.Spec{
					Command:     shim,
					CommandMode: adapterprocess.CommandModeWindowsCommandShim,
					Dir:         workDir,
				})
				return err
			},
		},
		{
			name: "stream",
			run: func() error {
				_, err := adapterprocess.RunStream(context.Background(), adapterprocess.Spec{
					Command:     shim,
					CommandMode: adapterprocess.CommandModeWindowsCommandShim,
					Dir:         workDir,
				}, nil)
				return err
			},
		},
		{
			name: "start",
			run: func() error {
				_, err := adapterprocess.Start(context.Background(), adapterprocess.StartSpec{
					Command:     shim,
					CommandMode: adapterprocess.CommandModeWindowsCommandShim,
					Dir:         workDir,
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "Windows command-shim mode is unavailable") {
				t.Fatalf("error = %v, want platform rejection", err)
			}
		})
	}
}

func TestStreamCallbackFailureKillsDescendantProcessGroup(t *testing.T) {
	mutationPath := filepath.Join(t.TempDir(), "late-stream-mutation")
	readyPath := filepath.Join(t.TempDir(), "stream-descendant-ready")
	command, args := helperCommand("spawn-descendant", mutationPath, readyPath)

	_, err := adapterprocess.RunStream(context.Background(), adapterprocess.Spec{
		Command: command,
		Args:    args,
		Dir:     t.TempDir(),
		Env: adapterprocess.EnvPolicy{Set: map[string]string{
			"GO_WANT_PROCESS_HELPER": "1",
		}},
	}, func([]byte) error {
		return errors.New("stream consumer stopped")
	})
	if err == nil || err.Error() != "stream process stdout: stream consumer stopped" {
		t.Fatalf("RunStream error = %v, want callback failure", err)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("descendant was not confirmed before stream failure: %v", err)
	}

	time.Sleep(650 * time.Millisecond)
	if _, statErr := os.Stat(mutationPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant mutated workspace after stream failure: %v", statErr)
	}
}

func TestLeaderExitRacingCancellationKillsDescendantProcessGroup(t *testing.T) {
	mutationPath := filepath.Join(t.TempDir(), "late-racy-mutation")
	readyPath := filepath.Join(t.TempDir(), "racy-descendant-ready")
	releasePath := filepath.Join(t.TempDir(), "release-leader")
	command, args := helperCommand("spawn-descendant-racy-exit", mutationPath, readyPath, releasePath)
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
	waitForProcessHelperFile(t, readyPath)
	if err := os.WriteFile(releasePath, []byte("exit"), 0o600); err != nil {
		t.Fatalf("release group leader: %v", err)
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want success or context cancellation at the exit race", err)
	}

	time.Sleep(650 * time.Millisecond)
	if _, statErr := os.Stat(mutationPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant mutated workspace after leader-exit cancellation race: %v", statErr)
	}
}

func waitForProcessHelperFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat process helper signal: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process helper did not signal readiness at %s", path)
}
