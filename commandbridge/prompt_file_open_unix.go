//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package commandbridge

import (
	"errors"
	"os"
	"syscall"
)

// openPromptResource prevents a final-component symlink swap and avoids
// blocking if a regular file is replaced by a FIFO between lstat and open.
func openPromptResource(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("create file from descriptor")
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
