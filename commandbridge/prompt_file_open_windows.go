//go:build windows

package commandbridge

import (
	"errors"
	"os"
	"syscall"
)

// openPromptResource opens the final path component itself instead of
// following a reparse point installed between lstat and open.
func openPromptResource(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	closeHandle := true
	defer func() {
		if closeHandle {
			_ = syscall.CloseHandle(handle)
		}
	}()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, err
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errors.New("local resource is a reparse point")
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, errors.New("local resource is a directory")
	}
	fileType, err := syscall.GetFileType(handle)
	if err != nil {
		return nil, err
	}
	if fileType != syscall.FILE_TYPE_DISK {
		return nil, errors.New("local resource is not a regular disk file")
	}

	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.New("create file from handle")
	}
	closeHandle = false
	return file, nil
}
