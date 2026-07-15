//go:build darwin

package commandbridge

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"unsafe"

	"golang.org/x/sys/unix"
)

var darwinACLEntryLine = regexp.MustCompile(`(?m)^\s*[0-9]+:`)

func verifyPromptResourceAncestorFile(file *os.File) error {
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	return verifyDarwinPromptResourceACLAbsent(file)
}

func securePromptResourceDirectoryFile(file *os.File, mode os.FileMode) error {
	if file == nil {
		return errors.New("prompt resource directory handle is nil")
	}
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	if err := stripDarwinPromptResourceACL(file); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	return verifyPromptResourceDirectoryFile(file)
}

func verifyPromptResourceDirectoryFile(file *os.File) error {
	if file == nil {
		return errors.New("prompt resource directory handle is nil")
	}
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	owner, err := fileOwnerUID(info)
	if err != nil {
		return err
	}
	if !info.IsDir() || owner != uint32(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
		return errors.New("prompt resource stage ownership or mode is not private")
	}
	return verifyDarwinPromptResourceACLAbsent(file)
}

func securePrivatePromptResourceFile(file *os.File, mode os.FileMode) error {
	if file == nil {
		return errors.New("staged resource file is nil")
	}
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	if err := stripDarwinPromptResourceACL(file); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	return verifyPrivatePromptResourceFile(file)
}

func verifyPrivatePromptResourceFile(file *os.File) error {
	if file == nil {
		return errors.New("staged resource file is nil")
	}
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	owner, err := fileOwnerUID(info)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || owner != uint32(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
		return errors.New("staged resource file ownership or mode is not private")
	}
	return verifyDarwinPromptResourceACLAbsent(file)
}

func stripDarwinPromptResourceACL(file *os.File) error {
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	path, err := darwinPromptResourceHandlePath(file)
	if err != nil {
		return err
	}
	cmd := exec.Command("/bin/chmod", "-N", path)
	cmd.Env = []string{"LC_ALL=C"}
	if err := cmd.Run(); err != nil {
		return errors.New("strip extended ACL")
	}
	if err := verifyDarwinPromptResourceHandlePath(file, path); err != nil {
		return err
	}
	return verifyDarwinPromptResourceACLAbsent(file)
}

func verifyDarwinPromptResourceACLAbsent(file *os.File) error {
	if err := verifyDarwinPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	path, err := darwinPromptResourceHandlePath(file)
	if err != nil {
		return err
	}
	cmd := exec.Command("/bin/ls", "-lde", path)
	cmd.Env = []string{"LC_ALL=C"}
	output, err := cmd.Output()
	if err != nil {
		return errors.New("inspect extended ACL")
	}
	if err := verifyDarwinPromptResourceHandlePath(file, path); err != nil {
		return err
	}
	if darwinACLEntryLine.Match(output) {
		return errors.New("prompt resource retains an extended ACL")
	}
	return nil
}

func verifyDarwinPromptResourceLocalFilesystem(file *os.File) error {
	if file == nil {
		return errors.New("prompt resource filesystem handle is nil")
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return errors.New("inspect prompt resource filesystem")
	}
	if !darwinPromptResourceFilesystemIsLocal(stat.Flags) {
		return errors.New("prompt resource filesystem is not local")
	}
	return nil
}

func darwinPromptResourceFilesystemIsLocal(flags uint32) bool {
	return flags&unix.MNT_LOCAL != 0
}

func darwinPromptResourceHandlePath(file *os.File) (string, error) {
	if file == nil {
		return "", errors.New("prompt resource handle is nil")
	}
	// F_GETPATH requires a MAXPATHLEN-sized buffer. 4096 exceeds Darwin's
	// current MAXPATHLEN while leaving room for future expansion.
	buffer := make([]byte, 4096)
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, file.Fd(), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buffer[0])))
	if errno != 0 {
		return "", errno
	}
	if nul := bytes.IndexByte(buffer, 0); nul >= 0 {
		buffer = buffer[:nul]
	}
	path := filepath.Clean(string(buffer))
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("prompt resource handle did not resolve to an absolute path")
	}
	if err := verifyDarwinPromptResourceHandlePath(file, path); err != nil {
		return "", err
	}
	return path, nil
}

func verifyDarwinPromptResourceHandlePath(file *os.File, path string) error {
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, pathInfo) {
		return errors.New("prompt resource path changed while inspecting its ACL")
	}
	return nil
}
