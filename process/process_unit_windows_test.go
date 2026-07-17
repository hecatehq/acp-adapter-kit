//go:build windows

package process_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	adapterprocess "github.com/hecatehq/acp-adapter-kit/process"
)

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
