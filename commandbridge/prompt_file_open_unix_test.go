//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package commandbridge

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenPromptResourceDoesNotFollowSymlinkOrBlockOnFIFO(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.txt")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if file, err := openPromptResource(symlink); err == nil {
		_ = file.Close()
		t.Fatal("openPromptResource followed a symlink")
	}

	fifo := filepath.Join(dir, "input.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := make(chan *os.File, 1)
	failed := make(chan error, 1)
	go func() {
		file, err := openPromptResource(fifo)
		if err != nil {
			failed <- err
			return
		}
		opened <- file
	}()
	select {
	case file := <-opened:
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			t.Fatal("FIFO reported as a regular file")
		}
	case err := <-failed:
		t.Fatalf("openPromptResource returned error for FIFO: %v", err)
	case <-time.After(time.Second):
		t.Fatal("openPromptResource blocked on FIFO")
	}
}
