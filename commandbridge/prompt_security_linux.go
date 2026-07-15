//go:build linux

package commandbridge

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var linuxPromptResourceACLAttributes = []string{
	"system.posix_acl_access",
	"system.posix_acl_default",
}

func verifyPromptResourceAncestorFile(file *os.File) error {
	if file == nil {
		return errors.New("prompt resource ancestor handle is nil")
	}
	if err := verifyLinuxPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	for _, attribute := range linuxPromptResourceACLAttributes {
		if size, getErr := unix.Fgetxattr(int(file.Fd()), attribute, nil); getErr == nil || size > 0 {
			return fmt.Errorf("prompt resource ancestor retains %s", attribute)
		} else if !linuxPromptResourceACLUnavailable(getErr) {
			return fmt.Errorf("verify ancestor %s absence: %w", attribute, getErr)
		}
	}
	return nil
}

func securePromptResourceDirectoryFile(file *os.File, mode os.FileMode) error {
	if file == nil {
		return errors.New("prompt resource directory handle is nil")
	}
	if err := verifyLinuxPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	for _, attribute := range linuxPromptResourceACLAttributes {
		if err := unix.Fremovexattr(int(file.Fd()), attribute); err != nil && !linuxPromptResourceACLUnavailable(err) {
			return fmt.Errorf("remove %s: %w", attribute, err)
		}
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
	if err := verifyLinuxPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("prompt resource stage is not a directory")
	}
	owner, err := fileOwnerUID(info)
	if err != nil {
		return err
	}
	if owner != uint32(os.Geteuid()) {
		return errors.New("prompt resource stage is not owned by the process user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("prompt resource stage permissions are not private")
	}
	for _, attribute := range linuxPromptResourceACLAttributes {
		if size, getErr := unix.Fgetxattr(int(file.Fd()), attribute, nil); getErr == nil || size > 0 {
			return fmt.Errorf("prompt resource stage retains %s", attribute)
		} else if !linuxPromptResourceACLUnavailable(getErr) {
			return fmt.Errorf("verify %s absence: %w", attribute, getErr)
		}
	}
	return nil
}

func securePrivatePromptResourceFile(file *os.File, mode os.FileMode) error {
	if file == nil {
		return errors.New("staged resource file is nil")
	}
	if err := verifyLinuxPromptResourceLocalFilesystem(file); err != nil {
		return err
	}
	if err := unix.Fremovexattr(int(file.Fd()), "system.posix_acl_access"); err != nil && !linuxPromptResourceACLUnavailable(err) {
		return fmt.Errorf("remove staged resource ACL: %w", err)
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
	if err := verifyLinuxPromptResourceLocalFilesystem(file); err != nil {
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
	if size, getErr := unix.Fgetxattr(int(file.Fd()), "system.posix_acl_access", nil); getErr == nil || size > 0 {
		return errors.New("staged resource file retains a POSIX ACL")
	} else if !linuxPromptResourceACLUnavailable(getErr) {
		return fmt.Errorf("verify staged resource ACL absence: %w", getErr)
	}
	return nil
}

func verifyLinuxPromptResourceLocalFilesystem(file *os.File) error {
	if file == nil {
		return errors.New("prompt resource filesystem handle is nil")
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect prompt resource filesystem: %w", err)
	}
	magic := uint32(stat.Type)
	if !linuxPromptResourceFilesystemAllowed(magic) {
		return fmt.Errorf("prompt resource filesystem type %#x is not in the trusted local allowlist", magic)
	}
	return nil
}

func linuxPromptResourceFilesystemAllowed(magic uint32) bool {
	switch magic {
	case uint32(unix.EXT4_SUPER_MAGIC): // ext2, ext3, and ext4 share this magic.
		return true
	case uint32(unix.XFS_SUPER_MAGIC),
		unix.BTRFS_SUPER_MAGIC,
		unix.TMPFS_MAGIC,
		unix.OVERLAYFS_SUPER_MAGIC,
		unix.RAMFS_MAGIC,
		unix.F2FS_SUPER_MAGIC:
		return true
	default:
		return false
	}
}

func linuxPromptResourceACLUnavailable(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
