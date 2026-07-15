//go:build windows

package commandbridge

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPromptResourceWindowsRejectsReparsePointAndNonDiskFile(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openPromptResource(regular)
	if err != nil {
		t.Fatalf("open regular file: %v", err)
	}
	contents, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil {
		t.Fatalf("read regular file: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close regular file: %v", closeErr)
	}
	if string(contents) != "contents" {
		t.Fatalf("read %q, want contents", contents)
	}

	symlink := filepath.Join(dir, "link.txt")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Logf("symlink unavailable: %v", err)
	} else if file, err := openPromptResource(symlink); err == nil {
		_ = file.Close()
		t.Fatal("openPromptResource followed a reparse point")
	}

	if file, err := openPromptResource("NUL"); err == nil {
		_ = file.Close()
		t.Fatal("openPromptResource accepted a non-disk device")
	}
}
