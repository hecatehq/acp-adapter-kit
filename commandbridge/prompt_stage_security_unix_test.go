//go:build darwin || linux

package commandbridge

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hecatehq/acp-adapter-kit/runtimeacp"
)

func TestPreparePromptResourcesRejectsUnsafeAncestor(t *testing.T) {
	root := t.TempDir()
	unsafeParent := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatal(err)
	}
	_, stage, err := preparePromptResources(context.Background(), oneImagePrompt(), PromptResourceLimits{}, unsafeParent, nil)
	var stagingErr *promptResourceStagingError
	if stage != nil || !errors.As(err, &stagingErr) || stagingErr.operation != "validate prompt resource temporary parent" {
		t.Fatalf("prepare result stage=%#v err=%v, want operational unsafe-ancestor rejection", stage, err)
	}
}

func TestPromptResourceStageGuardRetriesTransientCleanupInCall(t *testing.T) {
	prepared, stage, err := preparePromptResources(context.Background(), oneImagePrompt(), PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil || len(prepared.Prompt) != 1 || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	anchor := stage.anchor
	attempts := 0
	stage.cleanupHook = func(path string) error {
		attempts++
		if attempts == 1 {
			return errors.New("transient failure")
		}
		return nil
	}
	if err := stage.cleanup(); err != nil {
		t.Fatalf("bounded cleanup retry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", attempts)
	}
	if _, err := os.Stat(anchor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private anchor still exists after bounded retry: %v", err)
	}
}

func TestPromptResourceCleanupDoesNotMutateReplacementPermissions(t *testing.T) {
	_, stage, err := preparePromptResources(context.Background(), oneImagePrompt(), PromptResourceLimits{}, t.TempDir(), nil)
	if err != nil || stage == nil {
		t.Fatalf("prepare result stage=%#v err=%v", stage, err)
	}
	originalPath := stage.dir
	movedPath := originalPath + "-moved"
	if err := os.Rename(originalPath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(originalPath, 0o555); err != nil {
		t.Fatal(err)
	}
	guard := stage.guard.(*unixPromptResourceStageGuard)
	if err := guard.Cleanup(nil); err == nil {
		t.Fatal("cleanup succeeded after stage replacement")
	}
	replacement, err := os.Stat(originalPath)
	if err != nil {
		t.Fatalf("stat replacement: %v", err)
	}
	if got := replacement.Mode().Perm(); got != 0o555 {
		t.Fatalf("replacement mode = %o, want unchanged 555", got)
	}
	if err := securePromptResourceDirectoryFile(guard.stage.file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := guard.close(); err != nil {
		t.Fatal(err)
	}
	stage.guard = nil
	if err := os.Chmod(stage.anchor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stage.anchor); err != nil {
		t.Fatalf("test cleanup: %v", err)
	}
}

func oneImagePrompt() runtimeacp.PromptParams {
	return runtimeacp.PromptParams{Prompt: []runtimeacp.ContentBlock{{
		Type:     "image",
		MimeType: "image/png",
		Data:     base64.StdEncoding.EncodeToString([]byte("image")),
	}}}
}
