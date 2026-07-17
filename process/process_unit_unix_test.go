//go:build !windows

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

func TestContextCancellationKillsDescendantProcessGroup(t *testing.T) {
	mutationPath := filepath.Join(t.TempDir(), "late-mutation")
	command, args := helperCommand("spawn-descendant", mutationPath)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := adapterprocess.Run(ctx, adapterprocess.Spec{
		Command: command,
		Args:    args,
		Dir:     t.TempDir(),
		Env: adapterprocess.EnvPolicy{Set: map[string]string{
			"GO_WANT_PROCESS_HELPER": "1",
		}},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want context deadline", err)
	}

	time.Sleep(650 * time.Millisecond)
	if _, statErr := os.Stat(mutationPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant mutated workspace after cancellation: %v", statErr)
	}
}

func TestStreamCallbackFailureKillsDescendantProcessGroup(t *testing.T) {
	mutationPath := filepath.Join(t.TempDir(), "late-stream-mutation")
	command, args := helperCommand("spawn-descendant", mutationPath)

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

	time.Sleep(650 * time.Millisecond)
	if _, statErr := os.Stat(mutationPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant mutated workspace after stream failure: %v", statErr)
	}
}
